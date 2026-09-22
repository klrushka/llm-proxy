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

from backends import Entity  # noqa: E402
from model_worker import Worker, build_worker, create_server  # noqa: E402

from jsonschema import Draft202012Validator

_SCHEMA_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "inference_response.schema.json")


def _load_schema() -> dict:
    with open(_SCHEMA_PATH, encoding="utf-8") as fh:
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

    def infer(self, text: str) -> list[Entity]:
        self.infer_calls += 1
        return [Entity(label=self._label, start=0, end=len(text), confidence=0.9, model=self.name)]


def _make_fake(name: str, label: str, created: list[_FakeBackend]):
    def loader() -> _FakeBackend:
        backend = _FakeBackend(name, label)
        backend.load_calls += 1
        created.append(backend)
        return backend

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


if __name__ == "__main__":
    unittest.main()