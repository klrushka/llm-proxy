"""Smoke and contract tests for the model worker.

Uses only the standard library plus the pinned ``jsonschema`` test dependency.
No network, GPU, transformers, torch, gliner2, or model downloads: test-local
fakes are injected through the build_worker seam.

The POST /infer success response is validated against the checked-in JSON
Schema contract ``inference_response.schema.json`` (Draft 2020-12).
"""

from __future__ import annotations

import json
import os
import socket
import sys
import threading
import unittest
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from backends import (  # noqa: E402
    GLINER_LABELS,
    GLiNERBackend,
    MAX_CAPACITY,
    RUBERT_MODEL_ID,
    Entity,
    RuBERTBackend,
)
from model_worker import Worker, build_worker, create_server  # noqa: E402

from jsonschema import Draft202012Validator


def _load_schema(filename: str = "inference_response.schema.json") -> dict:
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), filename)
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def _validate_schema(schema: dict) -> None:
    Draft202012Validator.check_schema(schema)


def _validate_instance(schema: dict, instance: dict) -> list[str]:
    validator = Draft202012Validator(schema)
    return sorted(error.message for error in validator.iter_errors(instance))


class _FakeBackend:
    name: str

    def __init__(self, name: str, label: str) -> None:
        self.name = name
        self._label = label
        self.load_calls = 0
        self.infer_calls = 0
        self.count_calls = 0

    def infer(self, text: str) -> list[Entity]:
        self.infer_calls += 1
        return [Entity(label=self._label, start=0, end=len(text), confidence=0.9, model=self.name)]

    def count_tokens(self, text: str) -> int:
        """Fake tokenizer with semantics distinct from char/word counting.

        Each word contributes one token plus one token per two characters
        (subword-like). For "Иван Петров" this yields 7, while len(text) is 11
        and the word count is 2, so a test can prove the tokenizer path is
        used rather than a character or word counter.
        """
        self.count_calls += 1
        if text == "":
            return 0
        return sum(1 + (len(word) + 1) // 2 for word in text.split())

    def _effective_capacity(self) -> int:
        return 510

    def plan_windows(self, text: str, overlap_tokens: int) -> tuple[int, list[dict]]:
        if text == "":
            return 0, []
        count = self.count_tokens(text)
        return count, [{"start": 0, "end": len(text), "token_count": count}]


def _make_fake(name: str, label: str, created: list[_FakeBackend]):
    def loader() -> _FakeBackend:
        backend = _FakeBackend(name, label)
        backend.load_calls += 1
        created.append(backend)
        return backend

    return loader


class _FakeCountBackend:
    """Backend whose count_tokens returns a caller-controlled value."""

    name = "rubert"

    def __init__(self, count_result) -> None:
        self._count_result = count_result

    def infer(self, text: str) -> list[Entity]:
        return []

    def count_tokens(self, text: str):
        return self._count_result


def _make_count_fake(count_result):
    def loader() -> _FakeCountBackend:
        return _FakeCountBackend(count_result)

    return loader


class _ConcurrencyBackend:
    """Backend that blocks inside infer until released, tracking peak concurrency."""

    name = "rubert"

    def __init__(self) -> None:
        self.cond = threading.Condition()
        self.active = 0
        self.peak = 0
        self.release = threading.Event()

    def infer(self, text: str) -> list[Entity]:
        with self.cond:
            self.active += 1
            self.peak = max(self.peak, self.active)
            self.cond.notify_all()
        self.release.wait(timeout=10)
        with self.cond:
            self.active -= 1
        return []

    def count_tokens(self, text: str) -> int:
        return 0


class _FlakyBackend:
    """Backend that raises on the first call, then succeeds."""

    name = "rubert"

    def __init__(self) -> None:
        self.calls = 0

    def infer(self, text: str) -> list[Entity]:
        self.calls += 1
        if self.calls == 1:
            raise RuntimeError("boom")
        return []

    def count_tokens(self, text: str) -> int:
        return 0


def _wait_active(backend: _ConcurrencyBackend, n: int, timeout: float = 5) -> bool:
    with backend.cond:
        return backend.cond.wait_for(lambda: backend.active >= n, timeout=timeout)


def _post(port: int, path: str, payload: dict) -> tuple[int, dict]:
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}{path}",
        data=data,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as err:
        return err.code, json.loads(err.read().decode("utf-8"))


def _post_with_headers(port: int, path: str, payload: dict) -> tuple[int, dict, dict]:
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}{path}",
        data=data,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, dict(resp.headers), json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as err:
        return err.code, dict(err.headers), json.loads(err.read().decode("utf-8"))


def _post_raw(port: int, path: str, data: bytes, content_length: str | None = None) -> tuple[int, dict]:
    headers = {"Content-Type": "application/json"}
    if content_length is not None:
        headers["Content-Length"] = content_length
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}{path}",
        data=data,
        headers=headers,
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as err:
        return err.code, json.loads(err.read().decode("utf-8"))


def _post_async(port: int, path: str, payload: dict, results: dict, index: int) -> None:
    results[index] = _post(port, path, payload)


def _raw_http_request(port: int, request_bytes: bytes, timeout: float = 5) -> bytes:
    with socket.create_connection(("127.0.0.1", port), timeout=timeout) as sock:
        sock.sendall(request_bytes)
        sock.shutdown(socket.SHUT_WR)
        data = b""
        while True:
            chunk = sock.recv(4096)
            if not chunk:
                break
            data += chunk
    return data


def _parse_raw_response(data: bytes) -> tuple[int, bytes]:
    head, _, body = data.partition(b"\r\n\r\n")
    status_line = head.split(b"\r\n", 1)[0]
    status = int(status_line.split(b" ")[1])
    return status, body


def _parse_raw_headers(data: bytes) -> tuple[int, dict, bytes]:
    head, _, body = data.partition(b"\r\n\r\n")
    lines = head.split(b"\r\n")
    status = int(lines[0].split(b" ")[1])
    headers: dict = {}
    for line in lines[1:]:
        if b":" in line:
            key, _, value = line.partition(b":")
            headers[key.decode("latin-1").strip().lower()] = value.decode("latin-1").strip()
    return status, headers, body


def _default_tokenize(text: str) -> tuple[list[int], list[tuple[int, int]]]:
    """One token per two Unicode code points.

    Token count therefore differs from both character count and word count,
    and offsets land on code-point boundaries.
    """
    n = len(text)
    ids = list(range(1, (n + 1) // 2 + 1))
    offsets = [(i, min(i + 2, n)) for i in range(0, n, 2)]
    return ids, offsets


class _FakeTokenizer:
    """Configurable tokenizer fake exposing the transformers surface used."""

    def __init__(
        self,
        model_max_length: int = 512,
        special_tokens: int = 2,
        tokenize_fn=_default_tokenize,
    ) -> None:
        self.model_max_length = model_max_length
        self._special = special_tokens
        self._fn = tokenize_fn
        self.calls = 0
        self.code_points = 0

    def num_special_tokens_to_add(self, pair: bool = False) -> int:
        return self._special

    def __call__(self, text, add_special_tokens, truncation, return_offsets_mapping):
        self.calls += 1
        self.code_points += len(text)
        ids, offsets = self._fn(text)
        result = {"input_ids": ids}
        if return_offsets_mapping:
            result["offset_mapping"] = offsets
        return result


def _make_rubert_backend(tokenizer: _FakeTokenizer) -> RuBERTBackend:
    backend = RuBERTBackend.__new__(RuBERTBackend)
    backend._pipe = type("FakePipe", (), {"tokenizer": tokenizer})()
    return backend


def _threshold_tokenize(text: str) -> tuple[list[int], list[tuple[int, int]]]:
    """Context-dependent tokenizer used to force a re-tokenization shrink.

    Long inputs tokenize two code points per token; short inputs one code point
    per token. A substring re-tokenized alone can therefore exceed the capacity
    implied by the whole-text prefix, exercising the safe shrink path.
    """
    n = len(text)
    step = 2 if n > 10 else 1
    ids = list(range(1, (n + step - 1) // step + 1))
    offsets = [(i, min(i + step, n)) for i in range(0, n, step)]
    return ids, offsets


def _whitespace_skip_tokenize(text: str) -> tuple[list[int], list[tuple[int, int]]]:
    """One token per non-whitespace run; whitespace emits no token.

    Mirrors a real tokenizer that produces no tokens for spaces/newlines, so a
    trailing whitespace tail is untokenized and must be absorbed by the last
    window rather than producing zero-token windows.
    """
    ids: list[int] = []
    offsets: list[tuple[int, int]] = []
    n = len(text)
    i = 0
    tok_id = 1
    while i < n:
        if text[i].isspace():
            i += 1
            continue
        j = i
        while j < n and not text[j].isspace():
            j += 1
        ids.append(tok_id)
        offsets.append((i, j))
        tok_id += 1
        i = j
    return ids, offsets


def _adversarial_tokenize(text: str) -> tuple[list[int], list[tuple[int, int]]]:
    """Context-dependent tokenizer that forces a large naive shrink.

    The last 1000 code points are tokenized densely (one token per code point);
    the preceding region is tokenized sparsely (one token per 100 code points).
    A prefix re-tokenized alone therefore yields far more tokens than the sparse
    global offsets imply, so a naive one-code-point shrink would walk thousands
    of code points while a bounded token-boundary search stays cheap. The token
    count is monotonic in the boundary, so binary search is well-defined.
    """
    n = len(text)
    ids: list[int] = []
    offsets: list[tuple[int, int]] = []
    tok_id = 1
    dense_start = max(0, n - 1000)
    i = 0
    while i < dense_start:
        j = min(i + 100, dense_start)
        ids.append(tok_id)
        offsets.append((i, j))
        tok_id += 1
        i = j
    while i < n:
        ids.append(tok_id)
        offsets.append((i, i + 1))
        tok_id += 1
        i += 1
    return ids, offsets


def _tail_context_tokenize(text: str) -> tuple[list[int], list[tuple[int, int]]]:
    """Context-dependent tokenizer where a trailing whitespace tail changes density.

    A leading ``M`` marker forces sparse tokenization (one token per two code
    points). Otherwise, trailing whitespace forces dense tokenization (one token
    per code point); without trailing whitespace the content is sparse. The full
    text (with marker) is sparse, a later window without the marker and without
    the tail is sparse and fits, but the same window with the trailing tail is
    dense and exceeds capacity, so absorbing the tail must fail closed.
    """
    n = len(text)
    if n > 0 and text[0] == "M":
        step = 2
    elif n > 0 and text[-1].isspace():
        step = 1
    else:
        step = 2
    ids: list[int] = []
    offsets: list[tuple[int, int]] = []
    tok_id = 1
    i = 0
    while i < n:
        if text[i].isspace():
            i += 1
            continue
        j = i
        while j < n and not text[j].isspace():
            j += 1
        k = i
        while k < j:
            e = min(k + step, j)
            ids.append(tok_id)
            offsets.append((k, e))
            tok_id += 1
            k = e
        i = j
    return ids, offsets


class WorkerTest(unittest.TestCase):
    def test_build_worker_invokes_each_loader_exactly_once(self) -> None:
        created: list[_FakeBackend] = []
        load_rubert = _make_fake("rubert", "FULL_NAME", created)
        load_gliner = _make_fake("gliner", "ru_pii_person", created)

        worker = build_worker(load_rubert, load_gliner)

        self.assertEqual(len(created), 2)
        self.assertEqual(worker.models(), ["gliner", "rubert"])

    def test_loader_failure_prevents_worker_construction(self) -> None:
        def bad_loader() -> _FakeBackend:
            raise RuntimeError("model load failed")

        with self.assertRaises(RuntimeError):
            build_worker(bad_loader, _make_fake("gliner", "ru_pii_person", []))


class GLiNERLabelSetTest(unittest.TestCase):
    """Fixes the exact requested GLiNER label set; the generic ru_pii is absent."""

    def test_gliner_labels_exact_set(self) -> None:
        self.assertEqual(
            GLINER_LABELS,
            [
                "ru_pii_person",
                "ru_pii_location",
                "ru_pii_date",
                "ru_pii_phone",
                "ru_pii_email",
            ],
        )

    def test_gliner_labels_exclude_generic_ru_pii(self) -> None:
        self.assertNotIn("ru_pii", GLINER_LABELS)


class CountTokensTest(unittest.TestCase):
    def test_count_tokens_uses_rubert_tokenizer_not_char_or_word_count(self) -> None:
        created: list[_FakeBackend] = []
        worker = build_worker(
            _make_fake("rubert", "FULL_NAME", created),
            _make_fake("gliner", "ru_pii_person", created),
        )

        text = "Иван Петров"
        count = worker.count_tokens(text)

        # Fake tokenizer semantics: 1 + ceil(len/2) per word -> 3 + 4 = 7.
        self.assertEqual(count, 7)
        self.assertNotEqual(count, len(text))
        self.assertNotEqual(count, len(text.split()))

    def test_count_tokens_is_bound_to_rubert_backend_only(self) -> None:
        created: list[_FakeBackend] = []
        worker = build_worker(
            _make_fake("rubert", "FULL_NAME", created),
            _make_fake("gliner", "ru_pii_person", created),
        )

        worker.count_tokens("Иван Петров")

        by_name = {b.name: b for b in created}
        self.assertEqual(by_name["rubert"].count_calls, 1)
        self.assertEqual(by_name["gliner"].count_calls, 0)

    def test_count_tokens_empty_text_is_zero(self) -> None:
        created: list[_FakeBackend] = []
        worker = build_worker(
            _make_fake("rubert", "FULL_NAME", created),
            _make_fake("gliner", "ru_pii_person", created),
        )

        self.assertEqual(worker.count_tokens(""), 0)


class RuBERTBackendCountTokensTest(unittest.TestCase):
    """Direct unit test of RuBERTBackend.count_tokens without transformers.

    The backend is created via __new__ so no pipeline/model is loaded. A fake
    pipe/tokenizer is injected to prove the already-loaded pipeline tokenizer is
    reused, called with add_special_tokens=False and truncation=False, and that
    the returned token id count differs from characters and words.
    """

    def test_count_tokens_reuses_pipeline_tokenizer(self) -> None:
        calls: dict = {}

        class FakeTokenizer:
            def __call__(self, text, add_special_tokens, truncation, return_offsets_mapping):
                calls["text"] = text
                calls["add_special_tokens"] = add_special_tokens
                calls["truncation"] = truncation
                calls["return_offsets_mapping"] = return_offsets_mapping
                return {"input_ids": [1, 2, 3, 4, 5, 6, 7]}

        class FakePipe:
            tokenizer = FakeTokenizer()

        backend = RuBERTBackend.__new__(RuBERTBackend)
        backend._pipe = FakePipe()

        text = "Иван Петров"
        count = backend.count_tokens(text)

        self.assertEqual(count, 7)
        self.assertEqual(calls["text"], text)
        self.assertIs(calls["add_special_tokens"], False)
        self.assertIs(calls["truncation"], False)
        self.assertIs(calls["return_offsets_mapping"], True)
        self.assertNotEqual(count, len(text))
        self.assertNotEqual(count, len(text.split()))


class ServerTest(unittest.TestCase):
    def setUp(self) -> None:
        self.created: list[_FakeBackend] = []
        self.load_rubert = _make_fake("rubert", "FULL_NAME", self.created)
        self.load_gliner = _make_fake("gliner", "ru_pii_person", self.created)
        self.worker = build_worker(self.load_rubert, self.load_gliner)
        self.server = create_server(self.worker, 0)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def _get(self, path: str) -> tuple[int, dict]:
        with urllib.request.urlopen(f"http://127.0.0.1:{self.port}{path}", timeout=5) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))

    def _post(self, path: str, payload: dict) -> tuple[int, dict]:
        data = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(
            f"http://127.0.0.1:{self.port}{path}",
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                return resp.status, json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as err:
            return err.code, json.loads(err.read().decode("utf-8"))

    def _post_raw(self, path: str, data: bytes, content_length: str | None = None, port: int | None = None) -> tuple[int, dict]:
        port = port or self.port
        headers = {"Content-Type": "application/json"}
        if content_length is not None:
            headers["Content-Length"] = content_length
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}{path}",
            data=data,
            headers=headers,
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                return resp.status, json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as err:
            return err.code, json.loads(err.read().decode("utf-8"))

    def test_default_host_is_loopback(self) -> None:
        # The safe local default must bind loopback only, never 0.0.0.0.
        self.assertEqual(self.server.server_address[0], "127.0.0.1")

    def test_explicit_host_binds_all_interfaces(self) -> None:
        # A container must be able to bind 0.0.0.0 so the Go service can reach
        # the worker over the internal Docker network.
        server = create_server(self.worker, 0, host="0.0.0.0")
        port = server.server_address[1]
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            self.assertEqual(server.server_address[0], "0.0.0.0")
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=5) as resp:
                self.assertEqual(resp.status, 200)
                body = json.loads(resp.read().decode("utf-8"))
            self.assertEqual(body["status"], "ok")
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def test_health_reports_both_backends(self) -> None:
        status, body = self._get("/health")
        self.assertEqual(status, 200)
        self.assertEqual(body["status"], "ok")
        self.assertEqual(sorted(body["models"]), ["gliner", "rubert"])

    def test_two_inference_requests_reuse_same_instances(self) -> None:
        status1, body1 = self._post("/infer", {"text": "Иван Петров"})
        status2, body2 = self._post("/infer", {"text": "Иван Петров"})

        self.assertEqual(status1, 200)
        self.assertEqual(status2, 200)
        self.assertEqual(body1, body2)

        self.assertEqual(len(self.created), 2)
        for backend in self.created:
            self.assertEqual(backend.infer_calls, 2)

    def test_output_contains_only_safe_fields(self) -> None:
        _, body = self._post("/infer", {"text": "Иван Петров"})
        self.assertEqual(len(body["entities"]), 2)
        for entity in body["entities"]:
            self.assertEqual(set(entity.keys()), {"label", "start", "end", "confidence", "model"})
            self.assertNotIn("text", entity)

    def test_invalid_payload_returns_error(self) -> None:
        status, body = self._post("/infer", {"text": 123})
        self.assertEqual(status, 400)
        self.assertIn("error", body)

    def test_count_tokens_returns_tokenizer_count(self) -> None:
        status, body = self._post("/count_tokens", {"text": "Иван Петров"})
        self.assertEqual(status, 200)
        self.assertEqual(body, {"model": RUBERT_MODEL_ID, "count": 7})

    def test_count_tokens_empty_text_is_zero(self) -> None:
        status, body = self._post("/count_tokens", {"text": ""})
        self.assertEqual(status, 200)
        self.assertEqual(body, {"model": RUBERT_MODEL_ID, "count": 0})

    def test_count_tokens_invalid_payload_returns_error(self) -> None:
        status, body = self._post("/count_tokens", {"text": 123})
        self.assertEqual(status, 400)
        self.assertIn("error", body)

    def test_count_tokens_does_not_echo_text(self) -> None:
        status, body = self._post("/count_tokens", {"text": "Иван Петров"})
        self.assertEqual(status, 200)
        self.assertNotIn("text", body)
        self.assertEqual(set(body.keys()), {"model", "count"})

    def test_count_tokens_rejects_non_object_payloads(self) -> None:
        for raw in (b"[]", b"null", b"42", b'"text"'):
            with self.subTest(raw=raw):
                status, body = self._post_raw("/count_tokens", raw)
                self.assertEqual(status, 400)
                self.assertIn("error", body)

    def test_count_tokens_rejects_missing_or_unknown_key(self) -> None:
        for payload in ({}, {"other": "x"}, {"text": "a", "extra": 1}):
            with self.subTest(payload=payload):
                status, body = self._post("/count_tokens", payload)
                self.assertEqual(status, 400)
                self.assertIn("error", body)

    def test_count_tokens_rejects_invalid_content_length(self) -> None:
        status, body = self._post_raw("/count_tokens", b'{"text":"a"}', content_length="not-a-number")
        self.assertEqual(status, 400)
        self.assertIn("error", body)

    def test_count_tokens_rejects_invalid_json(self) -> None:
        status, body = self._post_raw("/count_tokens", b"{not json")
        self.assertEqual(status, 400)
        self.assertIn("error", body)

    def test_count_tokens_rejects_invalid_count(self) -> None:
        for bad in (True, -1, "5", 1.5):
            with self.subTest(bad=bad):
                worker = build_worker(
                    _make_count_fake(bad),
                    _make_fake("gliner", "ru_pii_person", []),
                )
                server = create_server(worker, 0)
                port = server.server_address[1]
                thread = threading.Thread(target=server.serve_forever, daemon=True)
                thread.start()
                try:
                    status, body = self._post_raw(
                        "/count_tokens",
                        '{"text":"Иван Петров"}'.encode("utf-8"),
                        port=port,
                    )
                finally:
                    server.shutdown()
                    server.server_close()
                    thread.join(timeout=5)
                self.assertEqual(status, 500)
                self.assertIn("error", body)


class InferenceContractTest(unittest.TestCase):
    """Validates the POST /infer success response against the JSON Schema."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.schema = _load_schema()
        _validate_schema(cls.schema)

    def test_schema_is_valid_draft_2020_12(self) -> None:
        self.assertEqual(self.schema["$schema"], "https://json-schema.org/draft/2020-12/schema")

    def test_live_infer_response_matches_schema(self) -> None:
        created: list[_FakeBackend] = []
        worker = build_worker(
            _make_fake("rubert", "FULL_NAME", created),
            _make_fake("gliner", "ru_pii_person", created),
        )
        server = create_server(worker, 0)
        port = server.server_address[1]
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            data = json.dumps({"text": "Иван Петров"}).encode("utf-8")
            req = urllib.request.Request(
                f"http://127.0.0.1:{port}/infer",
                data=data,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=5) as resp:
                self.assertEqual(resp.status, 200)
                body = json.loads(resp.read().decode("utf-8"))
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

        errors = _validate_instance(self.schema, body)
        self.assertEqual(errors, [], f"live /infer body failed schema: {errors}")

    def test_rejects_source_text_field(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"entities": [{"label": "FULL_NAME", "start": 0, "end": 5, "confidence": 0.9, "model": "rubert", "text": "Иван"}]},
        )
        self.assertTrue(errors, "expected schema rejection for source text field")

    def test_rejects_tokenization_artifact_field(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"entities": [{"label": "FULL_NAME", "start": 0, "end": 5, "confidence": 0.9, "model": "rubert", "token": "<PII_FULL_NAME_1>"}]},
        )
        self.assertTrue(errors, "expected schema rejection for tokenization artifact field")

    def test_rejects_mapping_field(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"entities": [{"label": "FULL_NAME", "start": 0, "end": 5, "confidence": 0.9, "model": "rubert", "mapping": {"<PII_FULL_NAME_1>": "Иван"}}]},
        )
        self.assertTrue(errors, "expected schema rejection for mapping field")

    def test_rejects_key_or_secret_material_field(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"entities": [{"label": "FULL_NAME", "start": 0, "end": 5, "confidence": 0.9, "model": "rubert", "key": "supersecret"}]},
        )
        self.assertTrue(errors, "expected schema rejection for key/secret material field")

    def test_rejects_missing_required_field(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"entities": [{"label": "FULL_NAME", "start": 0, "end": 5, "confidence": 0.9}]},
        )
        self.assertTrue(errors, "expected schema rejection for missing required field")

    def test_rejects_invalid_field_type(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"entities": [{"label": "FULL_NAME", "start": "0", "end": 5, "confidence": 0.9, "model": "rubert"}]},
        )
        self.assertTrue(errors, "expected schema rejection for invalid field type")

    def test_rejects_confidence_out_of_range(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"entities": [{"label": "FULL_NAME", "start": 0, "end": 5, "confidence": 1.5, "model": "rubert"}]},
        )
        self.assertTrue(errors, "expected schema rejection for out-of-range confidence")

    def test_rejects_unknown_model_source(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"entities": [{"label": "FULL_NAME", "start": 0, "end": 5, "confidence": 0.9, "model": "other"}]},
        )
        self.assertTrue(errors, "expected schema rejection for unknown model source")

    def test_rejects_extra_top_level_field(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"entities": [], "tokenized_text": "Иван"},
        )
        self.assertTrue(errors, "expected schema rejection for extra top-level field")


class CountTokensContractTest(unittest.TestCase):
    """Validates the POST /count_tokens success response against the JSON Schema."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.schema = _load_schema("count_tokens_response.schema.json")
        _validate_schema(cls.schema)

    def test_schema_is_valid_draft_2020_12(self) -> None:
        self.assertEqual(self.schema["$schema"], "https://json-schema.org/draft/2020-12/schema")

    def test_live_count_tokens_response_matches_schema(self) -> None:
        created: list[_FakeBackend] = []
        worker = build_worker(
            _make_fake("rubert", "FULL_NAME", created),
            _make_fake("gliner", "ru_pii_person", created),
        )
        server = create_server(worker, 0)
        port = server.server_address[1]
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            data = json.dumps({"text": "Иван Петров"}).encode("utf-8")
            req = urllib.request.Request(
                f"http://127.0.0.1:{port}/count_tokens",
                data=data,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=5) as resp:
                self.assertEqual(resp.status, 200)
                body = json.loads(resp.read().decode("utf-8"))
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

        errors = _validate_instance(self.schema, body)
        self.assertEqual(errors, [], f"live /count_tokens body failed schema: {errors}")

    def test_rejects_negative_count(self) -> None:
        errors = _validate_instance(self.schema, {"model": RUBERT_MODEL_ID, "count": -1})
        self.assertTrue(errors, "expected schema rejection for negative count")

    def test_rejects_non_integer_count(self) -> None:
        errors = _validate_instance(self.schema, {"model": RUBERT_MODEL_ID, "count": "5"})
        self.assertTrue(errors, "expected schema rejection for non-integer count")

    def test_rejects_extra_top_level_field(self) -> None:
        errors = _validate_instance(self.schema, {"model": RUBERT_MODEL_ID, "count": 5, "text": "Иван"})
        self.assertTrue(errors, "expected schema rejection for extra top-level field")

    def test_rejects_missing_count(self) -> None:
        errors = _validate_instance(self.schema, {"model": RUBERT_MODEL_ID})
        self.assertTrue(errors, "expected schema rejection for missing count")

    def test_rejects_missing_model(self) -> None:
        errors = _validate_instance(self.schema, {"count": 5})
        self.assertTrue(errors, "expected schema rejection for missing model")

    def test_rejects_wrong_model(self) -> None:
        errors = _validate_instance(self.schema, {"model": "other/model", "count": 5})
        self.assertTrue(errors, "expected schema rejection for wrong model")

    def test_rejects_non_string_model(self) -> None:
        errors = _validate_instance(self.schema, {"model": 123, "count": 5})
        self.assertTrue(errors, "expected schema rejection for non-string model")


class PlanWindowsTest(unittest.TestCase):
    """Unit tests of the tokenizer-derived window plan on a fake tokenizer."""

    def test_token_count_differs_from_chars_and_words(self) -> None:
        backend = _make_rubert_backend(_FakeTokenizer())
        text = "Иван Петров"
        total_count, windows = backend.plan_windows(text, overlap_tokens=0)
        self.assertEqual(len(windows), 1)
        token_count = windows[0]["token_count"]
        self.assertNotEqual(token_count, len(text))
        self.assertNotEqual(token_count, len(text.split()))
        self.assertEqual(total_count, token_count)

    def test_capacity_accounts_for_special_tokens(self) -> None:
        backend = _make_rubert_backend(_FakeTokenizer(model_max_length=512, special_tokens=2))
        self.assertEqual(backend._effective_capacity(), 510)

    def test_unicode_and_emoji_offsets_are_code_points(self) -> None:
        backend = _make_rubert_backend(_FakeTokenizer())
        text = "😀😀😀абв"
        _, windows = backend.plan_windows(text, overlap_tokens=0)
        self.assertEqual(len(windows), 1)
        self.assertEqual(windows[0]["start"], 0)
        self.assertEqual(windows[0]["end"], len(text))

    def test_multiple_windows_overlap_no_gaps_exact_end(self) -> None:
        backend = _make_rubert_backend(_FakeTokenizer(model_max_length=6, special_tokens=2))
        text = "abcdefghij"
        _, windows = backend.plan_windows(text, overlap_tokens=1)
        self.assertEqual(windows, [
            {"start": 0, "end": 8, "token_count": 4},
            {"start": 6, "end": 10, "token_count": 2},
        ])
        self.assertEqual(windows[0]["start"], 0)
        self.assertEqual(windows[-1]["end"], len(text))
        for w in windows:
            self.assertLess(w["start"], w["end"])
        for prev, nxt in zip(windows, windows[1:]):
            self.assertLessEqual(nxt["start"], prev["end"])

    def test_every_substring_retokenized_within_capacity(self) -> None:
        backend = _make_rubert_backend(_FakeTokenizer(model_max_length=6, special_tokens=2))
        text = "abcdefghij"
        _, windows = backend.plan_windows(text, overlap_tokens=1)
        for w in windows:
            self.assertLessEqual(w["token_count"], 4)

    def test_retokenization_shrink_with_guaranteed_progress(self) -> None:
        backend = _make_rubert_backend(
            _FakeTokenizer(model_max_length=5, special_tokens=2, tokenize_fn=_threshold_tokenize)
        )
        text = "abcdefghijklmnopqrst"
        _, windows = backend.plan_windows(text, overlap_tokens=0)
        self.assertEqual(windows[0]["start"], 0)
        self.assertEqual(windows[0]["end"], 2)
        self.assertEqual(windows[0]["token_count"], 2)
        self.assertLessEqual(windows[0]["token_count"], 3)
        for w in windows:
            self.assertLessEqual(w["token_count"], 3)
            self.assertGreaterEqual(w["token_count"], 1)

    def test_empty_text_returns_empty_plan(self) -> None:
        backend = _make_rubert_backend(_FakeTokenizer())
        self.assertEqual(backend.plan_windows("", overlap_tokens=0), (0, []))

    def test_invalid_overlap_raises(self) -> None:
        backend = _make_rubert_backend(_FakeTokenizer(model_max_length=6, special_tokens=2))
        for bad in (-1, 4, True, "4"):
            with self.subTest(bad=bad):
                with self.assertRaises(ValueError):
                    backend.plan_windows("abcdefghij", overlap_tokens=bad)

    def test_unrealistic_capacity_fails_closed(self) -> None:
        backend = _make_rubert_backend(
            _FakeTokenizer(model_max_length=10**30, special_tokens=2)
        )
        with self.assertRaises(ValueError):
            backend.plan_windows("abcdefghij", overlap_tokens=0)

    def test_non_positive_capacity_fails_closed(self) -> None:
        backend = _make_rubert_backend(_FakeTokenizer(model_max_length=1, special_tokens=2))
        with self.assertRaises(ValueError):
            backend.plan_windows("abcdefghij", overlap_tokens=0)

    def test_worker_total_count_is_full_text_tokenizer_count(self) -> None:
        rubert = _make_rubert_backend(_FakeTokenizer(model_max_length=6, special_tokens=2))
        worker = Worker({"rubert": rubert, "gliner": _FakeBackend("gliner", "ru_pii_person")})
        text = "abcdefghij"
        total_count, capacity, windows = worker.plan_windows(text, overlap_tokens=1)
        self.assertEqual(total_count, 5)
        self.assertEqual(capacity, 4)
        self.assertEqual(len(windows), 2)
        self.assertNotEqual(total_count, len(windows))
        self.assertNotEqual(total_count, len(text))
        self.assertNotEqual(total_count, len(text.split()))

    def test_worker_total_count_empty_text_is_zero(self) -> None:
        rubert = _make_rubert_backend(_FakeTokenizer(model_max_length=6, special_tokens=2))
        worker = Worker({"rubert": rubert, "gliner": _FakeBackend("gliner", "ru_pii_person")})
        total_count, capacity, windows = worker.plan_windows("", overlap_tokens=1)
        self.assertEqual(total_count, 0)
        self.assertEqual(windows, [])

    def test_worker_total_count_not_inflated_by_overlap(self) -> None:
        rubert = _make_rubert_backend(_FakeTokenizer(model_max_length=6, special_tokens=2))
        worker = Worker({"rubert": rubert, "gliner": _FakeBackend("gliner", "ru_pii_person")})
        text = "abcdefghij"
        total_count, _, windows = worker.plan_windows(text, overlap_tokens=1)
        window_sum = sum(w["token_count"] for w in windows)
        self.assertEqual(len(windows), 2)
        self.assertGreater(window_sum, total_count)
        self.assertEqual(total_count, 5)

    def test_trailing_whitespace_absorbed_no_zero_token_windows(self) -> None:
        backend = _make_rubert_backend(
            _FakeTokenizer(tokenize_fn=_whitespace_skip_tokenize)
        )
        for text in ("abc def ghi", "abc def ghi ", "abc def ghi\n   "):
            with self.subTest(text=text):
                total_count, windows = backend.plan_windows(text, overlap_tokens=0)
                self.assertEqual(total_count, 3)
                self.assertEqual(len(windows), 1)
                self.assertEqual(windows[0]["start"], 0)
                self.assertEqual(windows[0]["end"], len(text))
                self.assertEqual(windows[0]["token_count"], 3)

    def test_whitespace_only_input_has_zero_total_and_no_windows(self) -> None:
        backend = _make_rubert_backend(
            _FakeTokenizer(tokenize_fn=_whitespace_skip_tokenize)
        )
        for text in ("   ", "\n\t  ", " \n \n "):
            with self.subTest(text=text):
                total_count, windows = backend.plan_windows(text, overlap_tokens=0)
                self.assertEqual(total_count, 0)
                self.assertEqual(windows, [])

    def test_overlapping_windows_strictly_increasing_no_gaps(self) -> None:
        backend = _make_rubert_backend(_FakeTokenizer(model_max_length=6, special_tokens=2))
        text = "abcdefghijklmnopqrstuvwxyz"
        _, windows = backend.plan_windows(text, overlap_tokens=2)
        self.assertGreater(len(windows), 1)
        for w in windows:
            self.assertGreaterEqual(w["token_count"], 1)
        for prev, nxt in zip(windows, windows[1:]):
            self.assertLess(prev["start"], nxt["start"])
            self.assertLess(prev["end"], nxt["end"])
            self.assertLessEqual(nxt["start"], prev["end"])
        self.assertEqual(windows[0]["start"], 0)
        self.assertEqual(windows[-1]["end"], len(text))

    def test_planning_100k_tokens_is_linear(self) -> None:
        tokenizer = _FakeTokenizer(model_max_length=102, special_tokens=2)
        backend = _make_rubert_backend(tokenizer)
        text = "a" * 200000
        total_count, windows = backend.plan_windows(text, overlap_tokens=16)
        self.assertEqual(total_count, 100000)
        self.assertGreater(len(windows), 1)
        # Full text tokenized once (200000 code points) plus bounded per-window
        # re-tokenization. A quadratic planner would scan the whole suffix per
        # window (~windows * text_len code points), far exceeding this linear
        # bound of a few times the text length.
        self.assertLess(tokenizer.code_points, 3 * len(text))
        self.assertLess(tokenizer.calls, 2 * len(windows) + 4)

    def test_adversarial_contextual_shrink_is_bounded(self) -> None:
        tokenizer = _FakeTokenizer(
            model_max_length=152,
            special_tokens=2,
            tokenize_fn=_adversarial_tokenize,
        )
        backend = _make_rubert_backend(tokenizer)
        text = "a" * 10000
        total_count, windows = backend.plan_windows(text, overlap_tokens=0)
        self.assertEqual(total_count, 1090)
        self.assertGreater(len(windows), 1)
        # Every window is tokenful and within capacity (150).
        for w in windows:
            self.assertGreaterEqual(w["token_count"], 1)
            self.assertLessEqual(w["token_count"], 150)
        # No gaps, strict progress, exact end.
        self.assertEqual(windows[0]["start"], 0)
        self.assertEqual(windows[-1]["end"], len(text))
        for prev, nxt in zip(windows, windows[1:]):
            self.assertLess(prev["start"], nxt["start"])
            self.assertLessEqual(nxt["start"], prev["end"])
        # A naive one-code-point shrink would walk thousands of code points per
        # window; the bounded token-boundary search keeps calls to O(log
        # capacity) per window.
        self.assertLess(tokenizer.calls, 20 * len(windows))

    def test_adversarial_minimal_candidate_over_capacity_fails_closed(self) -> None:
        tokenizer = _FakeTokenizer(
            model_max_length=7,
            special_tokens=2,
            tokenize_fn=_adversarial_tokenize,
        )
        backend = _make_rubert_backend(tokenizer)
        text = "a" * 10000
        with self.assertRaises(ValueError):
            backend.plan_windows(text, overlap_tokens=0)

    def test_adversarial_tail_absorption_oversized_fails_closed(self) -> None:
        tokenizer = _FakeTokenizer(
            model_max_length=52,
            special_tokens=2,
            tokenize_fn=_tail_context_tokenize,
        )
        backend = _make_rubert_backend(tokenizer)
        text = "M" + "a" * 475 + " " * 50
        # Preconditions: the full text (with marker) is sparse; a later window
        # without the marker and without the tail is sparse and fits; the same
        # window with the trailing tail is dense and exceeds capacity.
        self.assertEqual(backend.count_tokens(text), 238)
        self.assertEqual(backend.count_tokens(text[400:476]), 38)
        self.assertLessEqual(backend.count_tokens(text[400:476]), 50)
        self.assertEqual(backend.count_tokens(text[400:526]), 76)
        self.assertGreater(backend.count_tokens(text[400:526]), 50)
        # Absorbing the trailing tail re-tokenizes the last window densely and
        # exceeds capacity 50; the planner must fail closed rather than emit an
        # oversized window.
        with self.assertRaises(ValueError):
            backend.plan_windows(text, overlap_tokens=0)

    def test_exact_total_count_not_sum_of_overlapping_windows(self) -> None:
        backend = _make_rubert_backend(_FakeTokenizer(model_max_length=6, special_tokens=2))
        text = "abcdefghij"
        total_count, windows = backend.plan_windows(text, overlap_tokens=1)
        window_sum = sum(w["token_count"] for w in windows)
        self.assertEqual(total_count, 5)
        self.assertGreater(window_sum, total_count)


class GLiNERLongTest(unittest.TestCase):
    """GLiNER production inference must use the bounded long-input API."""

    def test_infer_uses_extract_entities_long_with_bounded_chunks(self) -> None:
        calls: dict = {}

        class FakeModel:
            def extract_entities_long(self, text, labels, **kwargs):
                calls["text"] = text
                calls["labels"] = labels
                calls["kwargs"] = kwargs
                return {"entities": {}}

        backend = GLiNERBackend.__new__(GLiNERBackend)
        backend._model = FakeModel()

        text = "Иван Петров"
        entities = backend.infer(text)

        self.assertEqual(entities, [])
        self.assertEqual(calls["text"], text)
        self.assertEqual(calls["labels"], GLINER_LABELS)
        self.assertEqual(calls["kwargs"]["chunk_size"], 384)
        self.assertEqual(calls["kwargs"]["chunk_overlap"], 64)
        self.assertEqual(calls["kwargs"]["threshold"], 0.5)
        self.assertIs(calls["kwargs"]["include_spans"], True)
        self.assertIs(calls["kwargs"]["include_confidence"], True)


class PlanWindowsContractTest(unittest.TestCase):
    """Validates the POST /plan_windows success response against the JSON Schema."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.schema = _load_schema("window_plan_response.schema.json")
        _validate_schema(cls.schema)

    def test_schema_is_valid_draft_2020_12(self) -> None:
        self.assertEqual(self.schema["$schema"], "https://json-schema.org/draft/2020-12/schema")

    def test_live_plan_windows_response_matches_schema(self) -> None:
        created: list[_FakeBackend] = []
        worker = build_worker(
            _make_fake("rubert", "FULL_NAME", created),
            _make_fake("gliner", "ru_pii_person", created),
        )
        server = create_server(worker, 0)
        port = server.server_address[1]
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            data = json.dumps({"text": "Иван Петров", "overlap_tokens": 64}).encode("utf-8")
            req = urllib.request.Request(
                f"http://127.0.0.1:{port}/plan_windows",
                data=data,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=5) as resp:
                self.assertEqual(resp.status, 200)
                body = json.loads(resp.read().decode("utf-8"))
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

        errors = _validate_instance(self.schema, body)
        self.assertEqual(errors, [], f"live /plan_windows body failed schema: {errors}")

    def test_rejects_source_text_field(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"model": RUBERT_MODEL_ID, "total_count": 1, "max_window_tokens": 510,
             "windows": [{"start": 0, "end": 5, "token_count": 3, "text": "Иван"}]},
        )
        self.assertTrue(errors, "expected schema rejection for source text field")

    def test_rejects_token_id_field(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"model": RUBERT_MODEL_ID, "total_count": 1, "max_window_tokens": 510,
             "windows": [{"start": 0, "end": 5, "token_count": 3, "token_ids": [1, 2, 3]}]},
        )
        self.assertTrue(errors, "expected schema rejection for token id field")

    def test_rejects_mapping_field(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"model": RUBERT_MODEL_ID, "total_count": 1, "max_window_tokens": 510,
             "windows": [{"start": 0, "end": 5, "token_count": 3, "mapping": {"a": "b"}}]},
        )
        self.assertTrue(errors, "expected schema rejection for mapping field")

    def test_rejects_extra_top_level_field(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"model": RUBERT_MODEL_ID, "total_count": 1, "max_window_tokens": 510,
             "windows": [], "text": "Иван"},
        )
        self.assertTrue(errors, "expected schema rejection for extra top-level field")

    def test_rejects_negative_total_count(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"model": RUBERT_MODEL_ID, "total_count": -1, "max_window_tokens": 510, "windows": []},
        )
        self.assertTrue(errors, "expected schema rejection for negative total_count")

    def test_rejects_non_positive_max_window_tokens(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"model": RUBERT_MODEL_ID, "total_count": 0, "max_window_tokens": 0, "windows": []},
        )
        self.assertTrue(errors, "expected schema rejection for non-positive max_window_tokens")

    def test_rejects_negative_window_offset(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"model": RUBERT_MODEL_ID, "total_count": 1, "max_window_tokens": 510,
             "windows": [{"start": -1, "end": 5, "token_count": 3}]},
        )
        self.assertTrue(errors, "expected schema rejection for negative window offset")

    def test_rejects_zero_token_window(self) -> None:
        errors = _validate_instance(
            self.schema,
            {"model": RUBERT_MODEL_ID, "total_count": 1, "max_window_tokens": 510,
             "windows": [{"start": 0, "end": 5, "token_count": 0}]},
        )
        self.assertTrue(errors, "expected schema rejection for zero-token window")


class PlanWindowsServerTest(unittest.TestCase):
    """Server-level strict request handling for POST /plan_windows."""

    def setUp(self) -> None:
        self.created: list[_FakeBackend] = []
        self.load_rubert = _make_fake("rubert", "FULL_NAME", self.created)
        self.load_gliner = _make_fake("gliner", "ru_pii_person", self.created)
        self.worker = build_worker(self.load_rubert, self.load_gliner)
        self.server = create_server(self.worker, 0)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def _post(self, path: str, payload: dict) -> tuple[int, dict]:
        data = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(
            f"http://127.0.0.1:{self.port}{path}",
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                return resp.status, json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as err:
            return err.code, json.loads(err.read().decode("utf-8"))

    def test_strict_request_requires_exact_fields(self) -> None:
        for payload in (
            {},
            {"text": "a"},
            {"overlap_tokens": 64},
            {"text": "a", "overlap_tokens": 64, "extra": 1},
            {"text": "a", "overlap_tokens": 64, "text2": "b"},
        ):
            with self.subTest(payload=payload):
                status, body = self._post("/plan_windows", payload)
                self.assertEqual(status, 400)
                self.assertIn("error", body)

    def test_rejects_non_string_text(self) -> None:
        status, body = self._post("/plan_windows", {"text": 123, "overlap_tokens": 64})
        self.assertEqual(status, 400)
        self.assertIn("error", body)

    def test_rejects_invalid_overlap_tokens(self) -> None:
        for bad in ("64", 64.5, True, -1):
            with self.subTest(bad=bad):
                status, body = self._post("/plan_windows", {"text": "a", "overlap_tokens": bad})
                self.assertEqual(status, 400)
                self.assertIn("error", body)

    def test_response_contains_no_text_or_token_ids(self) -> None:
        status, body = self._post("/plan_windows", {"text": "Иван Петров", "overlap_tokens": 64})
        self.assertEqual(status, 200)
        self.assertEqual(set(body.keys()), {"model", "total_count", "max_window_tokens", "windows"})
        self.assertNotIn("text", body)
        for window in body["windows"]:
            self.assertEqual(set(window.keys()), {"start", "end", "token_count"})
            self.assertNotIn("token_ids", window)
            self.assertNotIn("text", window)

    def test_http_total_count_is_full_text_tokenizer_count(self) -> None:
        rubert = _make_rubert_backend(_FakeTokenizer(model_max_length=6, special_tokens=2))
        worker = Worker({"rubert": rubert, "gliner": _FakeBackend("gliner", "ru_pii_person")})
        server = create_server(worker, 0)
        port = server.server_address[1]
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            data = json.dumps({"text": "abcdefghij", "overlap_tokens": 1}).encode("utf-8")
            req = urllib.request.Request(
                f"http://127.0.0.1:{port}/plan_windows",
                data=data,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=5) as resp:
                self.assertEqual(resp.status, 200)
                body = json.loads(resp.read().decode("utf-8"))
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

        self.assertEqual(body["total_count"], 5)
        self.assertEqual(body["max_window_tokens"], 4)
        self.assertEqual(len(body["windows"]), 2)
        self.assertNotEqual(body["total_count"], len(body["windows"]))
        self.assertNotEqual(body["total_count"], len("abcdefghij"))
        self.assertNotEqual(body["total_count"], len("abcdefghij".split()))


class ContentLengthTest(unittest.TestCase):
    """Strict, bounded Content-Length handling for POST endpoints."""

    def setUp(self) -> None:
        self.created: list[_FakeBackend] = []
        worker = build_worker(
            _make_fake("rubert", "FULL_NAME", self.created),
            _make_fake("gliner", "ru_pii_person", self.created),
        )
        self.server = create_server(worker, 0)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def test_missing_content_length_rejected(self) -> None:
        data = _raw_http_request(
            self.port,
            b"POST /count_tokens HTTP/1.1\r\n"
            b"Host: 127.0.0.1\r\n"
            b"Content-Type: application/json\r\n"
            b"\r\n"
            b'{"text":"a"}',
        )
        status, _ = _parse_raw_response(data)
        self.assertEqual(status, 400)

    def test_negative_content_length_rejected(self) -> None:
        status, body = _post_raw(self.port, "/count_tokens", b'{"text":"a"}', content_length="-5")
        self.assertEqual(status, 400)
        self.assertIn("error", body)

    def test_nonnumeric_content_length_rejected(self) -> None:
        status, body = _post_raw(self.port, "/count_tokens", b'{"text":"a"}', content_length="abc")
        self.assertEqual(status, 400)
        self.assertIn("error", body)

    def test_unicode_digit_content_length_rejected_and_server_continues(self) -> None:
        # Arabic-Indic digit one (U+0661): isdigit() is True but int() raises.
        # It must be rejected with a safe 400, not a traceback, and the server
        # must keep serving new connections afterwards.
        data = _raw_http_request(
            self.port,
            b"POST /count_tokens HTTP/1.1\r\n"
            b"Host: 127.0.0.1\r\n"
            b"Content-Type: application/json\r\n"
            b"Content-Length: \xd9\xa1\r\n"
            b"\r\n"
            b'{"text":"a"}',
        )
        status, _ = _parse_raw_response(data)
        self.assertEqual(status, 400)
        status, body = _post(self.port, "/count_tokens", {"text": "Иван Петров"})
        self.assertEqual(status, 200)
        self.assertIn("count", body)

    def test_duplicate_content_length_rejected(self) -> None:
        data = _raw_http_request(
            self.port,
            b"POST /count_tokens HTTP/1.1\r\n"
            b"Host: 127.0.0.1\r\n"
            b"Content-Type: application/json\r\n"
            b"Content-Length: 5\r\n"
            b"Content-Length: 10\r\n"
            b"\r\n"
            b'{"text":"a"}',
        )
        status, _ = _parse_raw_response(data)
        self.assertEqual(status, 400)

    def test_truncated_body_rejected(self) -> None:
        data = _raw_http_request(
            self.port,
            b"POST /count_tokens HTTP/1.1\r\n"
            b"Host: 127.0.0.1\r\n"
            b"Content-Type: application/json\r\n"
            b"Content-Length: 100\r\n"
            b"\r\n"
            b'{"text":"a"}',
        )
        status, _ = _parse_raw_response(data)
        self.assertEqual(status, 400)

    def test_oversize_body_returns_413(self) -> None:
        worker = build_worker(
            _make_fake("rubert", "FULL_NAME", []),
            _make_fake("gliner", "ru_pii_person", []),
        )
        server = create_server(worker, 0, max_body_bytes=1024)
        port = server.server_address[1]
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            data = b'{"text":"' + b"a" * 2000 + b'"}'
            status, body = _post_raw(port, "/count_tokens", data)
            self.assertEqual(status, 413)
            self.assertIn("error", body)
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def test_body_just_below_limit_processed(self) -> None:
        worker = build_worker(
            _make_fake("rubert", "FULL_NAME", []),
            _make_fake("gliner", "ru_pii_person", []),
        )
        server = create_server(worker, 0, max_body_bytes=1024)
        port = server.server_address[1]
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            status, body = _post(port, "/count_tokens", {"text": "a" * 900})
            self.assertEqual(status, 200)
            self.assertIn("count", body)
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)


class AdmissionControlTest(unittest.TestCase):
    """Global non-blocking admission control across expensive POST endpoints."""

    def setUp(self) -> None:
        self.backend = _ConcurrencyBackend()
        worker = Worker(
            {"rubert": self.backend, "gliner": _FakeBackend("gliner", "ru_pii_person")}
        )
        self.server = create_server(worker, 0, max_inflight=4)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self) -> None:
        self.backend.release.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def _get(self, path: str) -> tuple[int, dict]:
        with urllib.request.urlopen(f"http://127.0.0.1:{self.port}{path}", timeout=5) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))

    def test_peak_concurrency_capped_and_extra_gets_503(self) -> None:
        results: dict = {}
        threads = []
        for i in range(4):
            t = threading.Thread(
                target=_post_async, args=(self.port, "/infer", {"text": "x"}, results, i)
            )
            threads.append(t)
            t.start()
        self.assertTrue(_wait_active(self.backend, 4))

        status, headers, body = _post_with_headers(self.port, "/infer", {"text": "y"})
        self.assertEqual(status, 503)
        self.assertEqual(headers.get("Retry-After"), "1")
        self.assertIn("error", body)
        self.assertLessEqual(self.backend.peak, 4)

        self.backend.release.set()
        for t in threads:
            t.join(timeout=10)
        for i in range(4):
            self.assertEqual(results[i][0], 200)

        status, body = _post(self.port, "/infer", {"text": "z"})
        self.assertEqual(status, 200)

    def test_health_available_when_permits_busy(self) -> None:
        results: dict = {}
        threads = []
        for i in range(4):
            t = threading.Thread(
                target=_post_async, args=(self.port, "/infer", {"text": "x"}, results, i)
            )
            threads.append(t)
            t.start()
        self.assertTrue(_wait_active(self.backend, 4))

        status, body = self._get("/health")
        self.assertEqual(status, 200)
        self.assertEqual(body["status"], "ok")

        self.backend.release.set()
        for t in threads:
            t.join(timeout=10)


class PermitReleaseTest(unittest.TestCase):
    """A permit must be released even when the worker method raises."""

    def test_exception_releases_permit(self) -> None:
        backend = _FlakyBackend()
        worker = Worker(
            {"rubert": backend, "gliner": _FakeBackend("gliner", "ru_pii_person")}
        )
        server = create_server(worker, 0, max_inflight=1)
        port = server.server_address[1]
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            status1, body1 = _post(port, "/infer", {"text": "x"})
            self.assertEqual(status1, 500)
            self.assertIn("error", body1)
            status2, body2 = _post(port, "/infer", {"text": "y"})
            self.assertEqual(status2, 200)
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)


class SecretLeakTest(unittest.TestCase):
    """Error responses must never echo request body, text, or secret material."""

    CANARY = "PII-ERROR-CANARY-42"

    def setUp(self) -> None:
        self.backend = _ConcurrencyBackend()
        worker = Worker(
            {"rubert": self.backend, "gliner": _FakeBackend("gliner", "ru_pii_person")}
        )
        self.server = create_server(worker, 0, max_inflight=1, max_body_bytes=1024)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self) -> None:
        self.backend.release.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def test_invalid_json_does_not_echo_secret(self) -> None:
        data = b'{"text": "' + self.CANARY.encode() + b'"'
        status, body = _post_raw(self.port, "/count_tokens", data)
        self.assertEqual(status, 400)
        self.assertNotIn(self.CANARY, json.dumps(body))

    def test_oversize_does_not_echo_secret(self) -> None:
        data = b'{"text": "' + self.CANARY.encode() + b'"' + b"a" * 2000
        status, body = _post_raw(self.port, "/count_tokens", data)
        self.assertEqual(status, 413)
        self.assertNotIn(self.CANARY, json.dumps(body))

    def test_503_does_not_echo_secret(self) -> None:
        results: dict = {}
        t = threading.Thread(
            target=_post_async, args=(self.port, "/infer", {"text": "x"}, results, 0)
        )
        t.start()
        self.assertTrue(_wait_active(self.backend, 1))

        status, body = _post(self.port, "/infer", {"text": self.CANARY})
        self.assertEqual(status, 503)
        self.assertNotIn(self.CANARY, json.dumps(body))

        self.backend.release.set()
        t.join(timeout=10)


class ConnectionCloseTest(unittest.TestCase):
    """Early responses that leave the body unread must close the connection to
    prevent HTTP stream desynchronization / request smuggling on keep-alive."""

    def setUp(self) -> None:
        self.backend = _ConcurrencyBackend()
        worker = Worker(
            {"rubert": self.backend, "gliner": _FakeBackend("gliner", "ru_pii_person")}
        )
        self.server = create_server(worker, 0, max_inflight=1, max_body_bytes=1024)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self) -> None:
        self.backend.release.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def test_503_has_connection_close(self) -> None:
        results: dict = {}
        t = threading.Thread(
            target=_post_async, args=(self.port, "/infer", {"text": "x"}, results, 0)
        )
        t.start()
        self.assertTrue(_wait_active(self.backend, 1))

        status, headers, _ = _post_with_headers(self.port, "/infer", {"text": "y"})
        self.assertEqual(status, 503)
        self.assertEqual(headers.get("Connection"), "close")

        self.backend.release.set()
        t.join(timeout=10)

    def test_413_has_connection_close(self) -> None:
        body = b'{"text":"' + b"a" * 2000 + b'"}'
        request = (
            b"POST /count_tokens HTTP/1.1\r\n"
            b"Host: 127.0.0.1\r\n"
            b"Content-Type: application/json\r\n"
            b"Content-Length: " + str(len(body)).encode() + b"\r\n"
            b"\r\n" + body
        )
        data = _raw_http_request(self.port, request)
        status, headers, _ = _parse_raw_headers(data)
        self.assertEqual(status, 413)
        self.assertEqual(headers.get("connection"), "close")

    def test_malformed_content_length_closes_connection(self) -> None:
        data = _raw_http_request(
            self.port,
            b"POST /count_tokens HTTP/1.1\r\n"
            b"Host: 127.0.0.1\r\n"
            b"Content-Type: application/json\r\n"
            b"Connection: keep-alive\r\n"
            b"Content-Length: \xd9\xa1\r\n"
            b"\r\n"
            b'{"text":"a"}',
        )
        status, headers, _ = _parse_raw_headers(data)
        self.assertEqual(status, 400)
        self.assertEqual(headers.get("connection"), "close")

    def test_truncated_body_closes_connection(self) -> None:
        with socket.create_connection(("127.0.0.1", self.port), timeout=5) as sock:
            sock.sendall(
                b"POST /count_tokens HTTP/1.1\r\n"
                b"Host: 127.0.0.1\r\n"
                b"Content-Type: application/json\r\n"
                b"Connection: keep-alive\r\n"
                b"Content-Length: 100\r\n"
                b"\r\n"
                b'{"text":"a"}'
            )
            sock.shutdown(socket.SHUT_WR)
            data = b""
            while True:
                chunk = sock.recv(4096)
                if not chunk:
                    break
                data += chunk
        status, headers, _ = _parse_raw_headers(data)
        self.assertEqual(status, 400)
        self.assertEqual(headers.get("connection"), "close")


if __name__ == "__main__":
    unittest.main()
