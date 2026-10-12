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
    # PD disaggregation (§8). The bootstrap port must be one the peer
    # container can reach. The transfer backend is deliberately NOT set here:
    # the internal fork hardcodes Mooncake over RDMA, so the TCP variant this
    # allowlist once carried was a value SGLang would reject outright.
    "--disaggregation-transfer-backend",
    "--disaggregation-bootstrap-port",
    # The RDMA device list. It is how KV traffic is steered around an HCA whose
    # path to the peer is dead: on the H100 nodes individual cards in an
    # otherwise healthy subnet do not carry traffic, and removing those names
    # is what took the 1P1D pair from a hanging transfer to a working one.
    "--disaggregation-ib-device",
    "--trust-remote-code",
    "--disable-radix-cache",
    "--attention-backend",
    "--kv-cache-dtype",
    "--load-format",
    "--model-loader-extra-config",
    # GLM-5.3-Flash (Glm5NextForConditionalGeneration) verified configuration.
    # The DSA backends and deep_gemm MoE runner are what took single-stream
    # decoding from 9 to 113 tok/s on H100 (docs/glm53-h100-validation-2026-09-23.md).
    "--dsa-prefill-backend",
    "--dsa-decode-backend",
    "--moe-runner-backend",
    "--disable-custom-all-reduce",
    "--reasoning-parser",
    "--tool-call-parser",
    # GLM-5.3-Flash verified configuration (docs/glm53-sglang-recipes-2026-10-08.md,
    # docs/glm53-layout-c-benchmark-2026-10-08.md). Every one of these is
    # load-bearing on H100: the expert-parallel degree, the chat template
    # (GLM ships it as a standalone chat_template.jinja, so without this flag
    # warmup fails), and disabling the flashinfer autotuner, whose OOM is
    # reported as an illegal memory access rather than an out-of-memory error.
    "--ep-size",
    "--chat-template",
    "--context-length",
    # What the client asks for by name. Prefill and decode have to advertise
    # the same one or the Router treats them as two different models.
    "--served-model-name",
    # The KV cache report is what the Router's readiness view is built from;
    # without it a worker can be serving and still be counted as unavailable.
    "--enable-cache-report",
    "--disable-flashinfer-autotune",
    "--disable-cuda-graph",
    # 0.5.20 split the aggregate batch-size bound into a per-role pair. The
    # decode bucket is the one that has to be capped when graph capture eats
    # the memory the KV pool needs.
    "--cuda-graph-max-bs-decode",
    # The shared-expert fusion path is not exercised by the deep_gemm MoE
    # runner this model uses, and leaving it on changes the numerical path.
    "--disable-shared-experts-fusion",
    # Speculative decoding. Only usable in the aggregated layout: the upstream
    # cookbook states it does not start under PD disaggregation at this cut.
    "--speculative-algorithm",
    "--speculative-num-steps",
    "--speculative-eagle-topk",
    "--speculative-num-draft-tokens",
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
        # Operators write "--kv-cache-dtype bfloat16" or "--tp-size=8" with
        # equal ease; both must become separate argv entries for the child.
        flag = argument.split("=", 1)[0].split(None, 1)[0]
        if flag not in ALLOWED_EXTRA_FLAGS:
            raise ConfigurationError("extra argument %r is not allowed" % flag)
        validated.extend(shlex.split(argument))
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
