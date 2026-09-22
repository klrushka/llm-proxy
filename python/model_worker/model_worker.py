"""Minimal HTTP model worker for the PII NER service.

Loads both required model backends exactly once at startup and serves a narrow
health/inference surface. Orchestration and privacy decisions stay in Go; this
worker only returns safe entity values.
"""

from __future__ import annotations

import argparse
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Callable

from backends import Backend, GLiNERBackend, RuBERTBackend

REQUIRED_MODELS = ("rubert", "gliner")


class Worker:
    """Owns the loaded backend instances and reuses them for inference."""

    def __init__(self, backends: dict[str, Backend]) -> None:
        self._backends = backends

    def models(self) -> list[str]:
        return sorted(self._backends)

    def infer(self, text: str) -> list[dict]:
        entities: list[dict] = []
        for backend in self._backends.values():
            for entity in backend.infer(text):
                entities.append(
                    {
                        "label": entity.label,
                        "start": entity.start,
                        "end": entity.end,
                        "confidence": entity.confidence,
                        "model": entity.model,
                    }
                )
        return entities


def build_worker(
    load_rubert: Callable[[], Backend],
    load_gliner: Callable[[], Backend],
) -> Worker:
    """Invoke each loader exactly once and return a Worker. No inference probe."""
    backends = {
        "rubert": load_rubert(),
        "gliner": load_gliner(),
    }
    return Worker(backends)


def _load_rubert() -> Backend:
    return RuBERTBackend()


def _load_gliner() -> Backend:
    return GLiNERBackend()


def _json_error(status: int, message: str) -> tuple[int, dict]:
    return status, {"error": message}


class _Handler(BaseHTTPRequestHandler):
    worker: Worker

    def log_message(self, format: str, *args) -> None:  # noqa: A002
        return

    def do_GET(self) -> None:  # noqa: N802
        if self.path != "/health":
            self._send_json(*_json_error(404, "not found"))
            return
        self._send_json(200, {"status": "ok", "models": self.worker.models()})

    def do_POST(self) -> None:  # noqa: N802
        if self.path != "/infer":
            self._send_json(*_json_error(404, "not found"))
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            payload = json.loads(body)
        except (ValueError, json.JSONDecodeError):
            self._send_json(*_json_error(400, "invalid request"))
            return
        text = payload.get("text")
        if not isinstance(text, str):
            self._send_json(*_json_error(400, "text must be a string"))
            return
        try:
            entities = self.worker.infer(text)
        except Exception:
            self._send_json(*_json_error(500, "inference failed"))
            return
        self._send_json(200, {"entities": entities})

    def _send_json(self, status: int, payload: dict) -> None:
        data = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def create_server(worker: Worker, port: int) -> ThreadingHTTPServer:
    handler = type("BoundHandler", (_Handler,), {"worker": worker})
    return ThreadingHTTPServer(("127.0.0.1", port), handler)


def main() -> None:
    parser = argparse.ArgumentParser(description="PII NER model worker")
    parser.add_argument("--port", type=int, default=8000)
    args = parser.parse_args()

    worker = build_worker(_load_rubert, _load_gliner)
    server = create_server(worker, args.port)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()