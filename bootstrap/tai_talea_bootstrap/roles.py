"""Role configuration and SGLang command construction.

The bootstrap receives a role from the control plane and turns it into an
argument vector. Nothing else is accepted: no shell, no free form command line.
"""

from __future__ import annotations

import shlex

ROLES = ("prefill", "decode")

# Characters that must never appear in an argument handed to SGLang. The command
# is executed as a vector, but rejecting them early gives a clean 400 instead of
# a mysterious SGLang failure.
_FORBIDDEN = ("\x00", "\n", "\r")

# Extra arguments are restricted to a small allowlist of SGLang flags. The
# control plane only ever sends these, and a compromised caller cannot smuggle in
# an arbitrary switch.
ALLOWED_EXTRA_FLAGS = (
    "--enable-metrics",
    "--mem-fraction-static",
    "--max-running-requests",
    "--chunked-prefill-size",
    "--max-total-tokens",
    "--tp-size",
    "--dp-size",
    # PD disaggregation (§8). The transfer backend has to match what the
    # partner network supports - mooncake_tcp for overlays without RDMA - and
    # the bootstrap port must be one the peer container can reach.
    "--disaggregation-transfer-backend",
    "--disaggregation-bootstrap-port",
    "--trust-remote-code",
    "--disable-radix-cache",
    "--attention-backend",
    "--kv-cache-dtype",
)


class ConfigurationError(ValueError):
    """Raised when a role configuration cannot be turned into a command."""


def _clean(value, field, *, max_length=4096):
    if value is None:
        return ""
    if not isinstance(value, str):
        raise ConfigurationError("%s must be a string" % field)
    value = value.strip()
    if len(value) > max_length:
        raise ConfigurationError("%s exceeds %d characters" % (field, max_length))
    for forbidden in _FORBIDDEN:
        if forbidden in value:
            raise ConfigurationError("%s contains control characters" % field)
    return value


def validate_extra_args(extra_args):
    """Validate the extra SGLang arguments against the allowlist."""
    if not extra_args:
        return []
    if not isinstance(extra_args, (list, tuple)):
        raise ConfigurationError("extra_args must be a list")
    if len(extra_args) > 64:
        raise ConfigurationError("extra_args accepts at most 64 entries")

    validated = []
    for raw in extra_args:
        argument = _clean(raw, "extra_args entry", max_length=256)
        if not argument:
            continue
        flag = argument.split("=", 1)[0]
        if flag not in ALLOWED_EXTRA_FLAGS:
            raise ConfigurationError("extra argument %r is not allowed" % flag)
        validated.append(argument)
    return validated


def build_sglang_command(role, model_id, *, model_path="", host="0.0.0.0", port=31000,
                         python_executable="python3", extra_args=()):
    """Build the SGLang argument vector for one role.

    Prefill and Decode workers are started with ``--disaggregation-mode`` so the
    Router can keep them in separate pools (§8).
    """
    if role not in ROLES:
        raise ConfigurationError("role must be one of %s" % (ROLES,))
    model_id = _clean(model_id, "model_id", max_length=1024)
    if not model_id:
        raise ConfigurationError("model_id is required")
    model_path = _clean(model_path or model_id, "model_path")
    host = _clean(host, "host", max_length=255)
    python_executable = _clean(python_executable, "python_executable", max_length=1024)

    try:
        port = int(port)
    except (TypeError, ValueError):
        raise ConfigurationError("port must be an integer")
    if not 1 <= port <= 65535:
        raise ConfigurationError("port must be between 1 and 65535")

    command = [
        python_executable,
        "-m",
        "sglang.launch_server",
        "--model-path",
        model_path,
        "--host",
        host,
        "--port",
        str(port),
        "--disaggregation-mode",
        role,
    ]
    command += validate_extra_args(extra_args)
    # Note: there is deliberately no --log-file here. SGLang 0.5.x does not
    # accept one, and passing it made the child die inside argparse - before it
    # produced a single line of output. Capturing the service's output is the
    # bootstrap's job, done by redirecting the child's streams in _spawn.
    return command


def render_command(command):
    """Render a command for the audit log without executing it."""
    return " ".join(shlex.quote(part) for part in command)


__all__ = [
    "ROLES",
    "ALLOWED_EXTRA_FLAGS",
    "ConfigurationError",
    "build_sglang_command",
    "render_command",
    "validate_extra_args",
]
