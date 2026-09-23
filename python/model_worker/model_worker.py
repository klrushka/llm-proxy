"""Minimal HTTP model worker for the PII NER service.

Loads both required model backends exactly once at startup and serves a narrow
health/inference surface. Orchestration and privacy decisions stay in Go; this
worker only returns safe entity values.
"""

from __future__ import annotations

import argparse
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Callable

from backends import RUBERT_MODEL_ID, Backend, GLiNERBackend, RuBERTBackend

REQUIRED_MODELS = ("rubert", "gliner")

# Fixed JSON body cap: 8 MiB is comfortably enough for ~100k ordinary tokens.
MAX_BODY_BYTES = 8 * 1024 * 1024
# Global cap on concurrent expensive POST operations across all endpoints.
MAX_INFLIGHT = 4
# Per-connection socket read timeout in seconds.
SOCKET_TIMEOUT = 10
# Retry-After value (seconds) sent with 503 admission rejections.
RETRY_AFTER = 1
# Backlog for the listening socket.
REQUEST_QUEUE_SIZE = 128


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

    def count_tokens(self, text: str) -> int:
        """Count tokens with the RuBERT tokenizer only.

        The count path is bound specifically to the RuBERT backend identifier;
        GLiNER is never consulted for token counts.
        """
        return self._backends["rubert"].count_tokens(text)

    def plan_windows(self, text: str, overlap_tokens: int) -> tuple[int, int, list[dict]]:
        """Build a tokenizer-derived window plan with the RuBERT tokenizer only.

        Returns ``(total_count, max_window_tokens, windows)``. ``total_count``
        is the exact token count of the full source text produced by the same
        loaded RuBERT tokenizer during a single full-text tokenization, never
        the number of windows nor the sum of overlapping window counts. The plan
        carries only code-point ranges and per-window token counts; no text,
        token ids, token strings, or mappings.
        """
        total_count, windows = self._backends["rubert"].plan_windows(text, overlap_tokens)
        capacity = self._backends["rubert"]._effective_capacity()
        return total_count, capacity, windows


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
    _permit: threading.BoundedSemaphore
    max_body_bytes: int

    def log_message(self, format: str, *args) -> None:  # noqa: A002
        return

    def do_GET(self) -> None:  # noqa: N802
        if self.path != "/health":
            self._send_json(*_json_error(404, "not found"))
            return
        self._send_json(200, {"status": "ok", "models": self.worker.models()})

    def do_POST(self) -> None:  # noqa: N802
        if self.path not in ("/infer", "/count_tokens", "/plan_windows"):
            self._send_json(*_json_error(404, "not found"))
            return
        # Non-blocking admission control: reject before reading the body or
        # touching the worker so a saturated server never queues unbounded work.
        if not self._permit.acquire(blocking=False):
            self._send_retry_after()
            return
        try:
            self._handle_expensive(self.path)
        finally:
            self._permit.release()

    def _handle_expensive(self, path: str) -> None:
        if path == "/infer":
            self._handle_infer()
        elif path == "/count_tokens":
            self._handle_count_tokens()
        else:
            self._handle_plan_windows()

    def _read_json_body(self) -> tuple[dict | None, tuple[int, dict] | None]:
        """Read and parse a bounded JSON body with strict Content-Length checks.

        Returns ``(payload, None)`` on success or ``(None, (status, error))``.
        Never echoes request body, text, token ids, mappings, or exception
        detail in the returned error.
        """
        lengths = self.headers.get_all("Content-Length")
        if lengths is None or len(lengths) != 1:
            self.close_connection = True
            return None, (400, {"error": "invalid request"})
        raw = lengths[0].strip()
        # Accept only non-empty ASCII digits 0-9. isdigit() alone admits Unicode
        # digits that int() would reject; isascii() narrows it to ASCII first so
        # any malformed/Unicode/overflow-like header yields a safe 400 without
        # reading the body or raising.
        if not raw.isascii() or not raw.isdigit():
            self.close_connection = True
            return None, (400, {"error": "invalid request"})
        length = int(raw)
        if length > self.max_body_bytes:
            self.close_connection = True
            return None, (413, {"error": "request too large"})
        try:
            body = self.rfile.read(length)
        except OSError:
            self.close_connection = True
            return None, (400, {"error": "invalid request"})
        if len(body) != length:
            self.close_connection = True
            return None, (400, {"error": "invalid request"})
        try:
            payload = json.loads(body)
        except (ValueError, json.JSONDecodeError):
            return None, (400, {"error": "invalid request"})
        if not isinstance(payload, dict):
            return None, (400, {"error": "invalid request"})
        return payload, None

    def _handle_infer(self) -> None:
        payload, err = self._read_json_body()
        if err is not None:
            self._send_json(*err)
            return
        if set(payload.keys()) != {"text"}:
            self._send_json(*_json_error(400, "invalid request"))
            return
        text = payload["text"]
        if not isinstance(text, str):
            self._send_json(*_json_error(400, "text must be a string"))
            return
        try:
            entities = self.worker.infer(text)
        except Exception:
            self._send_json(*_json_error(500, "inference failed"))
            return
        self._send_json(200, {"entities": entities})

    def _handle_count_tokens(self) -> None:
        payload, err = self._read_json_body()
        if err is not None:
            self._send_json(*err)
            return
        if set(payload.keys()) != {"text"}:
            self._send_json(*_json_error(400, "invalid request"))
            return
        text = payload["text"]
        if not isinstance(text, str):
            self._send_json(*_json_error(400, "text must be a string"))
            return
        try:
            count = self.worker.count_tokens(text)
        except Exception:
            self._send_json(*_json_error(500, "token count failed"))
            return
        if isinstance(count, bool) or not isinstance(count, int) or count < 0:
            self._send_json(*_json_error(500, "token count failed"))
            return
        self._send_json(200, {"model": RUBERT_MODEL_ID, "count": count})

    def _handle_plan_windows(self) -> None:
        payload, err = self._read_json_body()
        if err is not None:
            self._send_json(*err)
            return
        if set(payload.keys()) != {"text", "overlap_tokens"}:
            self._send_json(*_json_error(400, "invalid request"))
            return
        text = payload["text"]
        if not isinstance(text, str):
            self._send_json(*_json_error(400, "text must be a string"))
            return
        overlap_tokens = payload["overlap_tokens"]
        if isinstance(overlap_tokens, bool) or not isinstance(overlap_tokens, int):
            self._send_json(*_json_error(400, "overlap_tokens must be an integer"))
            return
        if overlap_tokens < 0:
            self._send_json(*_json_error(400, "overlap_tokens must be non-negative"))
            return
        try:
            total_count, max_window_tokens, windows = self.worker.plan_windows(
                text, overlap_tokens
            )
        except Exception:
            self._send_json(*_json_error(500, "window plan failed"))
            return
        self._send_json(
            200,
            {
                "model": RUBERT_MODEL_ID,
                "total_count": total_count,
                "max_window_tokens": max_window_tokens,
                "windows": windows,
            },
        )

    def _send_retry_after(self) -> None:
        data = json.dumps({"error": "server busy"}).encode("utf-8")
        self.close_connection = True
        self.send_response(503)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Retry-After", str(RETRY_AFTER))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(data)

    def _send_json(self, status: int, payload: dict) -> None:
        data = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        if self.close_connection:
            self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(data)


class _Server(ThreadingHTTPServer):
    """Threading server with daemon threads, a bounded backlog, and a socket
    read timeout so a stalled client cannot pin a worker thread forever."""

    daemon_threads = True
    request_queue_size = REQUEST_QUEUE_SIZE

    def __init__(self, addr, handler, socket_timeout: int) -> None:
        super().__init__(addr, handler)
        self._socket_timeout = socket_timeout

    def get_request(self):
        request, client_address = super().get_request()
        request.settimeout(self._socket_timeout)
        return request, client_address


def create_server(
    worker: Worker,
    port: int,
    host: str = "127.0.0.1",
    *,
    max_inflight: int = MAX_INFLIGHT,
    max_body_bytes: int = MAX_BODY_BYTES,
    socket_timeout: int = SOCKET_TIMEOUT,
) -> _Server:
    """Create the hardened server.

    ``max_inflight``, ``max_body_bytes`` and ``socket_timeout`` are narrow
    overrides for tests; production callers keep the safe defaults.
    """
    permit = threading.BoundedSemaphore(max_inflight)
    handler = type(
        "BoundHandler",
        (_Handler,),
        {
            "worker": worker,
            "_permit": permit,
            "max_body_bytes": max_body_bytes,
        },
    )
    return _Server((host, port), handler, socket_timeout)


def main() -> None:
    parser = argparse.ArgumentParser(description="PII NER model worker")
    parser.add_argument("--port", type=int, default=8000)
    parser.add_argument(
        "--host",
        type=str,
        default="127.0.0.1",
        help="bind address; keep 127.0.0.1 outside Docker, use 0.0.0.0 in a container",
    )
    args = parser.parse_args()

    worker = build_worker(_load_rubert, _load_gliner)
    server = create_server(worker, args.port, args.host)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()