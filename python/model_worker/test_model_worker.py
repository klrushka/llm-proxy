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
import sys
import threading
import unittest
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from backends import RUBERT_MODEL_ID, Entity, RuBERTBackend  # noqa: E402
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
            def __call__(self, text, add_special_tokens, truncation):
                calls["text"] = text
                calls["add_special_tokens"] = add_special_tokens
                calls["truncation"] = truncation
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


if __name__ == "__main__":
    unittest.main()