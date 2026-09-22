"""Opt-in real-model smoke for the PII NER worker.

Loads the real pinned RuBERT and GLiNER2 backends exactly once through
build_worker, starts the existing HTTP server on an ephemeral localhost port,
and exercises GET /health and POST /infer against real inference.

Not part of the unit test suite; invoked explicitly and may download models.
"""

from __future__ import annotations

import json
import os
import sys
import threading
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from backends import GLiNERBackend, RuBERTBackend  # noqa: E402
from model_worker import build_worker, create_server  # noqa: E402

REQUIRED_MODELS = {"rubert", "gliner"}
SAFE_FIELDS = {"label", "start", "end", "confidence", "model"}

# Synthetic Russian sample covering a full name, email, phone, date, and address.
SYNTHETIC_SAMPLE = (
    "Иван Петров, email ivan.petrov@example.com, телефон +7 912 345-67-89, "
    "дата рождения 15.03.1985, адрес Москва, ул. Тверская, д. 1"
)


def _load_rubert() -> RuBERTBackend:
    return RuBERTBackend()


def _load_gliner() -> GLiNERBackend:
    return GLiNERBackend()


def _get(port: int, path: str) -> tuple[int, dict]:
    with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=120) as resp:
        return resp.status, json.loads(resp.read().decode("utf-8"))


def _post(port: int, path: str, payload: dict) -> tuple[int, dict]:
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}{path}",
        data=data,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=120) as resp:
        return resp.status, json.loads(resp.read().decode("utf-8"))


def _fail(message: str) -> None:
    raise SystemExit(f"SMOKE FAILED: {message}")


def main() -> None:
    print("Loading real backends (may download models on first run)...")
    worker = build_worker(_load_rubert, _load_gliner)

    server = create_server(worker, 0)
    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        status, body = _get(port, "/health")
        if status != 200:
            _fail(f"/health returned {status}")
        if body.get("status") != "ok":
            _fail(f"/health status not ok: {body}")
        if set(body.get("models", [])) != REQUIRED_MODELS:
            _fail(f"/health models {body.get('models')} != {sorted(REQUIRED_MODELS)}")

        status, body = _post(port, "/infer", {"text": SYNTHETIC_SAMPLE})
        if status != 200:
            _fail(f"/infer returned {status}: {body}")

        entities = body.get("entities")
        if not isinstance(entities, list):
            _fail(f"/infer entities is not a list: {body}")
        if not entities:
            _fail("/infer returned no entities")

        seen_models: set[str] = set()
        for entity in entities:
            if not isinstance(entity, dict):
                _fail(f"entity is not an object: {entity}")
            if set(entity.keys()) != SAFE_FIELDS:
                _fail(f"entity fields {sorted(entity.keys())} != {sorted(SAFE_FIELDS)}")
            if not isinstance(entity["label"], str) or not entity["label"]:
                _fail(f"entity label invalid: {entity}")
            if not isinstance(entity["start"], int) or entity["start"] < 0:
                _fail(f"entity start invalid: {entity}")
            if not isinstance(entity["end"], int) or entity["end"] < 0:
                _fail(f"entity end invalid: {entity}")
            if not (0 <= entity["start"] <= entity["end"] <= len(SYNTHETIC_SAMPLE)):
                _fail(f"entity bounds out of range: {entity}")
            if not isinstance(entity["confidence"], (int, float)) or not (0 <= entity["confidence"] <= 1):
                _fail(f"entity confidence out of range: {entity}")
            if entity["model"] not in REQUIRED_MODELS:
                _fail(f"entity model unknown: {entity}")
            seen_models.add(entity["model"])

        if seen_models != REQUIRED_MODELS:
            _fail(
                f"inference did not involve both backends; saw models "
                f"{sorted(seen_models)}, expected {sorted(REQUIRED_MODELS)}"
            )
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)

    print("SMOKE PASSED")


if __name__ == "__main__":
    main()