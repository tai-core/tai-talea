"""Local control interface of the bootstrap process (§9).

    GET  /bootstrap/health
    GET  /bootstrap/status
    POST /bootstrap/start
    POST /bootstrap/stop

Every endpoint requires the shared secret in ``X-Bootstrap-Token``. The token
comes from ``TAI_TALEA_BOOTSTRAP_TOKEN`` (or ``--token-file``) and the server
**refuses to start without it** rather than falling back to an open interface.

Two independent barriers keep the interface off the public internet:

1. the default bind address is loopback, so exposing the port is an explicit
   decision made by the deployment;
2. the shared secret, which holds even when the port *is* reachable by others.

Both are needed. Partner platforms routinely expose container ports publicly by
default (九章智算云 opens 9001/9002 to the internet and publishes a public
address), so "binds inside the container only" cannot be an assumption written
in a docstring - it has to be enforced here.

The server exposes no generic command execution surface: ``start`` accepts a
role, a model and an allowlisted set of SGLang flags, and nothing else.
"""

from __future__ import annotations

import hmac
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

# Shared secret header. The control plane sends the same value on every call.
HEADER_TOKEN = "X-Bootstrap-Token"

# Environment variable carrying the shared secret into the container.
ENV_TOKEN = "TAI_TALEA_BOOTSTRAP_TOKEN"

# Secure by default: exposing the interface beyond the container loopback is an
# explicit opt-in by the deployment (`--host 0.0.0.0`).
DEFAULT_HOST = "127.0.0.1"

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

    # ---------------------------------------------------------------- auth

    def _authorized(self):
        """True when the request carries the bootstrap shared secret.

        ``compare_digest`` keeps the comparison constant time so the token cannot
        be recovered byte by byte. A wrong token and a missing token answer
        identically: the response never says which part was wrong.
        """
        presented = self.headers.get(HEADER_TOKEN) or ""
        expected = self.server.token or ""
        return hmac.compare_digest(presented.encode("utf-8"), expected.encode("utf-8"))

    def _reject_unauthorized(self):
        logging.getLogger("tai_talea_bootstrap.server").warning(
            "rejected unauthenticated request: %s %s from %s",
            self.command, self.path, self.address_string(),
        )
        self._write(401, {
            "error": "unauthorized",
            "message": "%s is missing or invalid" % HEADER_TOKEN,
        })

    # ------------------------------------------------------------ routing

    def do_GET(self):  # noqa: N802 - http.server API
        if not self._authorized():
            self._reject_unauthorized()
            return
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
        if not self._authorized():
            self._reject_unauthorized()
            return
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
    """Threading HTTP server that owns the supervised service.

    A server constructed without a shared secret is refused outright. The
    alternative would be an interface that anyone able to reach the port can
    drive, which is exactly the failure this guards against.
    """

    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address, service, token):
        if not token or not str(token).strip():
            raise ValueError(
                "bootstrap shared secret is required: set %s or pass --token-file" % ENV_TOKEN
            )
        super().__init__(address, BootstrapHandler)
        self.service = service
        self.token = str(token)


def serve(service, host=DEFAULT_HOST, port=8080, token="", stop_event=None):
    """Run the bootstrap control interface until ``stop_event`` is set."""
    server = BootstrapServer((host, int(port)), service, token)
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
    "HEADER_TOKEN", "ENV_TOKEN", "DEFAULT_HOST",
    "BootstrapHandler", "BootstrapServer", "serve", "MAX_BODY_BYTES",
]
