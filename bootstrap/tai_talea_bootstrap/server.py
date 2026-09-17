"""Local control interface of the bootstrap process (§9).

    GET  /bootstrap/health
    GET  /bootstrap/status
    POST /bootstrap/start
    POST /bootstrap/stop

The server binds inside the container only. It exposes no generic command
execution surface: ``start`` accepts a role and an allowlisted set of SGLang
flags, and nothing else.
"""

from __future__ import annotations

import json
import logging
import signal
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from . import BOOTSTRAP_VERSION, exitcodes
from .roles import ConfigurationError
from .service import SGLangService

PATH_HEALTH = "/bootstrap/health"
PATH_STATUS = "/bootstrap/status"
PATH_START = "/bootstrap/start"
PATH_STOP = "/bootstrap/stop"

MAX_BODY_BYTES = 64 * 1024


class BootstrapHandler(BaseHTTPRequestHandler):
    """Request handler bound to one :class:`SGLangService`."""

    server_version = "tai-talea-bootstrap/" + BOOTSTRAP_VERSION
    protocol_version = "HTTP/1.1"

    # ----------------------------------------------------------- plumbing

    def log_message(self, fmt, *args):
        logging.getLogger("tai_talea_bootstrap.server").info(
            "%s - %s", self.address_string(), fmt % args
        )

    def _write(self, status, payload):
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read_json(self):
        try:
            length = int(self.headers.get("Content-Length") or "0")
        except ValueError:
            raise ConfigurationError("Content-Length is invalid")
        if length < 0 or length > MAX_BODY_BYTES:
            raise ConfigurationError("request body must not exceed %d bytes" % MAX_BODY_BYTES)
        raw = self.rfile.read(length) if length else b""
        if not raw:
            return {}
        try:
            payload = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            raise ConfigurationError("request body is not valid json: %s" % error)
        if not isinstance(payload, dict):
            raise ConfigurationError("request body must be a json object")
        return payload

    @property
    def service(self):
        return self.server.service

    # ------------------------------------------------------------ routing

    def do_GET(self):  # noqa: N802 - http.server API
        if self.path == PATH_HEALTH:
            health = self.service.health()
            status = health.get("status")
            health["version"] = BOOTSTRAP_VERSION
            self._write(200 if status in ("ok", "degraded") else 503, health)
            return
        if self.path == PATH_STATUS:
            payload = self.service.status()
            payload["version"] = BOOTSTRAP_VERSION
            self._write(200, payload)
            return
        self._write(404, {"error": "not_found", "message": "unknown path %s" % self.path})

    def do_POST(self):  # noqa: N802 - http.server API
        if self.path == PATH_START:
            self._handle_start()
            return
        if self.path == PATH_STOP:
            self._handle_stop()
            return
        self._write(404, {"error": "not_found", "message": "unknown path %s" % self.path})

    def _handle_start(self):
        try:
            payload = self._read_json()
        except ConfigurationError as error:
            self._write(400, {"error": "invalid_payload", "message": str(error)})
            return
        try:
            port = int(payload.get("port", 31000))
        except (TypeError, ValueError):
            self._write(400, {"error": "invalid_payload", "message": "port must be an integer"})
            return
        result = self.service.start(
            role=payload.get("role", ""),
            model_id=payload.get("model_id", ""),
            model_path=payload.get("model_path", ""),
            port=port,
            extra_args=payload.get("extra_args") or (),
            log_file=payload.get("log_file", ""),
            prepare_environment=bool(payload.get("prepare_environment", True)),
        )
        if result.accepted:
            self._write(202, {"accepted": True, "phase": result.phase, "detail": result.detail})
            return
        self._write(422, {
            "accepted": False,
            "phase": result.phase,
            "detail": result.detail,
            "exit_code": self.service.state.exit_code,
        })

    def _handle_stop(self):
        try:
            payload = self._read_json()
        except ConfigurationError as error:
            self._write(400, {"error": "invalid_payload", "message": str(error)})
            return
        try:
            timeout = float(payload.get("timeout", 60.0))
        except (TypeError, ValueError):
            self._write(400, {"error": "invalid_payload", "message": "timeout must be a number"})
            return
        if timeout < 0 or timeout > 3600:
            self._write(400, {"error": "invalid_payload", "message": "timeout must be between 0 and 3600"})
            return
        result = self.service.stop(timeout=timeout, force=bool(payload.get("force", False)))
        self._write(200, result.as_dict())


class BootstrapServer(ThreadingHTTPServer):
    """Threading HTTP server that owns the supervised service."""

    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address, service):
        super().__init__(address, BootstrapHandler)
        self.service = service


def serve(service, host="0.0.0.0", port=8080, stop_event=None):
    """Run the bootstrap control interface until ``stop_event`` is set."""
    server = BootstrapServer((host, int(port)), service)
    stop_event = stop_event or threading.Event()

    def _shutdown(_signum=None, _frame=None):
        stop_event.set()
        threading.Thread(target=server.shutdown, daemon=True).start()

    previous = {}
    for name in ("SIGTERM", "SIGINT"):
        number = getattr(signal, name, None)
        if number is None:
            continue
        try:
            previous[number] = signal.getsignal(number)
            signal.signal(number, _shutdown)
        except (ValueError, OSError):
            # Not on the main thread: the caller drives the stop event instead.
            pass

    try:
        server.serve_forever(poll_interval=0.2)
    finally:
        server.server_close()
        for number, handler in previous.items():
            try:
                signal.signal(number, handler)
            except (ValueError, OSError):
                pass
    return exitcodes.EXIT_OK


__all__ = [
    "PATH_HEALTH", "PATH_STATUS", "PATH_START", "PATH_STOP",
    "BootstrapHandler", "BootstrapServer", "serve", "MAX_BODY_BYTES",
]
