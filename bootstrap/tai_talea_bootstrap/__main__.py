"""Command line entry point of the container bootstrap process.

    python -m tai_talea_bootstrap serve   --profile /etc/tai-talea/profile.json
    python -m tai_talea_bootstrap check   --profile /etc/tai-talea/profile.json
    python -m tai_talea_bootstrap status

``serve`` is the mode a container runs: it validates the compatibility matrix,
exposes the four /bootstrap endpoints and forwards termination signals. It exits
with one of the documented exit codes after the service stopped (§9).
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sys
import threading

from . import BOOTSTRAP_VERSION, exitcodes
from .profile import ImageProfile, ProfileError, detect_environment
from .service import SGLangService
from .server import serve

DEFAULT_PROFILE_PATH = "/etc/tai-talea/profile.json"


def load_profile(path):
    """Read the controlled image profile from a json file."""
    if not path or not os.path.isfile(path):
        raise ProfileError(["profile file %s does not exist" % path])
    with open(path, "r", encoding="utf-8") as handle:
        payload = json.load(handle)
    if not isinstance(payload, dict):
        raise ProfileError(["profile must be a json object"])
    return ImageProfile(
        os_version=payload.get("os", ""),
        cuda=payload.get("cuda", ""),
        python=payload.get("python", ""),
        sglang=payload.get("sglang", ""),
        wheelhouse=payload.get("wheelhouse", ""),
        virtualenv=payload.get("virtualenv", ""),
        bootstrap_version=payload.get("bootstrap_version", BOOTSTRAP_VERSION),
        model_id=payload.get("model_id", ""),
    )


def _configure_logging(level):
    logging.basicConfig(
        level=getattr(logging, str(level).upper(), logging.INFO),
        format='{"ts":"%(asctime)s","level":"%(levelname)s","logger":"%(name)s","msg":%(message)s}',
        stream=sys.stdout,
    )


def command_check(args):
    profile = load_profile(args.profile)
    report = detect_environment(profile)
    print(json.dumps({
        "compatible": report.compatible,
        "environment": report.as_dict(),
        "mismatches": report.mismatches,
        "profile": profile.as_dict(),
    }, indent=2))
    return exitcodes.EXIT_OK if report.compatible else exitcodes.EXIT_ENV_INCOMPATIBLE


def command_status(args):
    """Report what a running bootstrap publishes, without importing requests."""
    import urllib.error
    import urllib.request

    url = "http://%s:%d/bootstrap/status" % (args.host, args.port)
    try:
        with urllib.request.urlopen(url, timeout=args.timeout) as response:  # noqa: S310
            print(response.read().decode("utf-8"))
            return exitcodes.EXIT_OK
    except (urllib.error.URLError, OSError) as error:
        print("bootstrap is not reachable at %s: %s" % (url, error), file=sys.stderr)
        return exitcodes.EXIT_INTERNAL


def command_serve(args):
    profile = load_profile(args.profile)
    report = detect_environment(profile)
    service = SGLangService(profile)
    service.note_environment(report)

    if not report.compatible:
        # A mismatched container must be released, not served (§10).
        print(json.dumps({
            "error": "environment_incompatible",
            "mismatches": report.mismatches,
        }), file=sys.stderr)
        return exitcodes.EXIT_ENV_INCOMPATIBLE

    stop_event = threading.Event()

    def _forward_signal(_signum, _frame):
        # Stop the service first, then let the http server drain.
        service.stop(timeout=args.stop_timeout)
        stop_event.set()

    import signal
    for name in ("SIGTERM", "SIGINT"):
        number = getattr(signal, name, None)
        if number is not None:
            try:
                signal.signal(number, _forward_signal)
            except (ValueError, OSError):
                pass

    logging.getLogger("tai_talea_bootstrap").info(
        "serving bootstrap %s on %s:%d", BOOTSTRAP_VERSION, args.host, args.port
    )
    try:
        serve(service, host=args.host, port=args.port, stop_event=stop_event)
    finally:
        if service.state.phase not in ("STOPPED", "FAILED"):
            service.stop(timeout=args.stop_timeout)

    code = service.state.exit_code
    if code == exitcodes.EXIT_OK and service.state.phase == "FAILED":
        code = exitcodes.EXIT_START_FAILED
    return code


def build_parser():
    parser = argparse.ArgumentParser(
        prog="tai_talea_bootstrap",
        description="Container bootstrap process for the SGLang capacity control plane.",
    )
    parser.add_argument("--version", action="version", version=BOOTSTRAP_VERSION)
    parser.add_argument("--log-level", default=os.environ.get("TAI_TALEA_BOOTSTRAP_LOG_LEVEL", "info"))
    subparsers = parser.add_subparsers(dest="command", required=True)

    serve_parser = subparsers.add_parser("serve", help="run the bootstrap control interface")
    serve_parser.add_argument("--profile", default=DEFAULT_PROFILE_PATH)
    serve_parser.add_argument("--host", default="0.0.0.0")
    serve_parser.add_argument("--port", type=int, default=8080)
    serve_parser.add_argument("--stop-timeout", type=float, default=60.0)
    serve_parser.set_defaults(func=command_serve)

    check_parser = subparsers.add_parser("check", help="validate the compatibility matrix and exit")
    check_parser.add_argument("--profile", default=DEFAULT_PROFILE_PATH)
    check_parser.set_defaults(func=command_check)

    status_parser = subparsers.add_parser("status", help="read the status of a running bootstrap")
    status_parser.add_argument("--host", default="127.0.0.1")
    status_parser.add_argument("--port", type=int, default=8080)
    status_parser.add_argument("--timeout", type=float, default=5.0)
    status_parser.set_defaults(func=command_status)
    return parser


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    _configure_logging(args.log_level)
    if not hasattr(args, "func"):
        parser.print_help()
        return exitcodes.EXIT_BAD_REQUEST
    try:
        return int(args.func(args))
    except ProfileError as error:
        print(json.dumps({"error": "profile_invalid", "mismatches": error.mismatches}), file=sys.stderr)
        return exitcodes.EXIT_ENV_INCOMPATIBLE
    except KeyboardInterrupt:
        return exitcodes.EXIT_OK
    except Exception as error:  # noqa: BLE001 - the exit code is the contract
        print(json.dumps({"error": "internal", "message": str(error)}), file=sys.stderr)
        return exitcodes.EXIT_INTERNAL


if __name__ == "__main__":
    sys.exit(main())
