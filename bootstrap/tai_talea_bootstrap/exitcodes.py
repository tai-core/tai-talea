"""Explicit exit codes returned by the bootstrap (§9).

The control plane records these values in the drain audit trail, so they are part
of the contract and must stay stable.
"""

# The service stopped cleanly after a stop request.
EXIT_OK = 0
# The role configuration or the request payload was invalid.
EXIT_BAD_REQUEST = 64
# The controlled image does not match the required compatibility matrix.
EXIT_ENV_INCOMPATIBLE = 65
# Preparing the Python environment from the offline wheelhouse failed.
EXIT_ENV_INSTALL_FAILED = 66
# SGLang could not be started.
EXIT_START_FAILED = 67
# SGLang exited on its own with a non zero status.
EXIT_SERVICE_CRASHED = 68
# The stop request timed out and the process had to be killed.
EXIT_STOP_TIMEOUT = 69
# An internal invariant was violated.
EXIT_INTERNAL = 70

EXIT_DESCRIPTIONS = {
    EXIT_OK: "service stopped cleanly",
    EXIT_BAD_REQUEST: "invalid role configuration or request payload",
    EXIT_ENV_INCOMPATIBLE: "container environment does not match the controlled image profile",
    EXIT_ENV_INSTALL_FAILED: "installing the offline wheelhouse into the virtualenv failed",
    EXIT_START_FAILED: "sglang could not be started",
    EXIT_SERVICE_CRASHED: "sglang exited unexpectedly",
    EXIT_STOP_TIMEOUT: "sglang did not stop before the deadline",
    EXIT_INTERNAL: "internal bootstrap error",
}


def describe(code):
    """Return a human readable description of an exit code."""
    return EXIT_DESCRIPTIONS.get(int(code), "unknown exit code")
