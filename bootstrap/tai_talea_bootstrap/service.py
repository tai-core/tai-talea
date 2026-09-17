"""Bootstrap state machine and SGLang process supervision.

Phases mirror what the control plane's InstanceManager expects to read from
``GET /bootstrap/status``: IDLE, INSTALLING, STARTING, RUNNING, STOPPING, STOPPED
and FAILED. Every phase change records why it happened, and the process always
ends with an explicit exit code (§9).
"""

from __future__ import annotations

import os
import signal
import subprocess
import threading
import time
from dataclasses import dataclass, field
from urllib.error import URLError
from urllib.request import urlopen

from . import exitcodes
from .environment import EnvironmentInstaller
from .roles import ConfigurationError, build_sglang_command, render_command

PHASE_IDLE = "IDLE"
PHASE_INSTALLING = "INSTALLING"
PHASE_STARTING = "STARTING"
PHASE_RUNNING = "RUNNING"
PHASE_STOPPING = "STOPPING"
PHASE_STOPPED = "STOPPED"
PHASE_FAILED = "FAILED"

PHASES = (PHASE_IDLE, PHASE_INSTALLING, PHASE_STARTING, PHASE_RUNNING,
          PHASE_STOPPING, PHASE_STOPPED, PHASE_FAILED)


@dataclass
class StartResult:
    accepted: bool
    phase: str
    detail: str = ""


@dataclass
class StopResult:
    phase: str
    exit_code: int
    timed_out: bool
    detail: str = ""

    def as_dict(self):
        return {
            "phase": self.phase,
            "exit_code": self.exit_code,
            "timed_out": self.timed_out,
            "detail": self.detail,
        }


@dataclass
class BootstrapState:
    """Everything ``/bootstrap/status`` reports."""

    phase: str = PHASE_IDLE
    role: str = ""
    model_id: str = ""
    service_pid: int = 0
    started_at: float = 0.0
    stopped_at: float = 0.0
    exit_code: int = 0
    last_error: str = ""
    environment: dict = field(default_factory=dict)
    command: str = ""
    started_count: int = 0
    stopped_count: int = 0
    history: list = field(default_factory=list)

    def record(self, phase, detail=""):
        self.phase = phase
        entry = {"phase": phase, "at": time.time(), "detail": detail}
        if self.last_error and phase == PHASE_FAILED:
            entry["error"] = self.last_error
        self.history.append(entry)
        if len(self.history) > 64:
            del self.history[:-64]

    def as_dict(self):
        return {
            "phase": self.phase,
            "role": self.role,
            "model_id": self.model_id,
            "service_pid": self.service_pid,
            "started_at": self.started_at,
            "stopped_at": self.stopped_at,
            "exit_code": self.exit_code,
            "last_error": self.last_error,
            "environment": self.environment,
            "history": self.history[-16:],
        }


# How long to watch a freshly spawned service before declaring it RUNNING.
# It only catches failures that happen while the interpreter is still coming up
# - a missing module, a rejected flag, a busy port. Anything slower is the
# control plane's job to find with the SGLang health check (§7.2), because the
# bootstrap must not hold the caller for long.
STARTUP_GRACE_SECONDS = 0.25


class SGLangService:
    """Owns the SGLang child process, the health probe and the exit code."""

    def __init__(self, profile, installer=None, runner=None, command_factory=None,
                 health_probe=None, now=None):
        self.profile = profile
        self.installer = installer or EnvironmentInstaller(profile)
        self._runner = runner
        self._command_factory = command_factory or build_sglang_command
        self._health_probe = health_probe or self._probe_http_health
        self._now = now or time.time
        self._lock = threading.RLock()
        self._process = None
        self.state = BootstrapState(environment=profile.as_dict())

    # ------------------------------------------------------------- queries

    def status(self):
        with self._lock:
            return self.state.as_dict()

    def health(self):
        """Health surface: the bootstrap is up, and whether SGLang answers."""
        with self._lock:
            phase = self.state.phase
            pid = self.state.service_pid
            port = self._port
        if phase == PHASE_FAILED:
            return {"status": "failed", "phase": phase, "detail": self.state.last_error}
        if phase != PHASE_RUNNING:
            return {"status": "ok", "phase": phase, "detail": "bootstrap is up"}
        healthy = self._health_probe(port) if pid else False
        return {
            "status": "ok" if healthy else "degraded",
            "phase": phase,
            "detail": "sglang answered the health probe" if healthy else "sglang did not answer yet",
            "service_id": str(pid),
        }

    @property
    def _port(self):
        return getattr(self, "_bound_port", 31000)

    # -------------------------------------------------------------- start

    def start(self, role, model_id, model_path="", port=31000, extra_args=(), log_file="",
              prepare_environment=True):
        """Prepare the environment and start SGLang for one role."""
        with self._lock:
            if self.state.phase in (PHASE_RUNNING, PHASE_STARTING, PHASE_INSTALLING):
                return StartResult(False, self.state.phase,
                                   "a service is already %s" % self.state.phase)
            self._bound_port = port
            self.state.role = role
            self.state.model_id = model_id
            self.state.last_error = ""
            self.state.exit_code = 0
            self.state.stopped_at = 0.0

        # The interpreter always comes from the managed virtualenv, which the
        # profile names. Preparing the environment only decides whether the
        # requirements are (re)installed. Defaulting to a bare "python3" meant
        # that a start with prepare_environment=False launched the *system*
        # interpreter - which has no sglang - so the child died instantly.
        python_executable = self.installer.python_executable
        try:
            if prepare_environment:
                with self._lock:
                    self.state.record(PHASE_INSTALLING, "preparing the offline virtualenv")
                python_executable = self.installer.prepare()
            command = self._command_factory(
                role, model_id, model_path=model_path, port=port,
                python_executable=python_executable, extra_args=extra_args,
            )
        except ConfigurationError as error:
            return self._fail(str(error), exitcodes.EXIT_BAD_REQUEST)
        except Exception as error:  # environment preparation failures
            return self._fail("environment preparation failed: %s" % error,
                              exitcodes.EXIT_ENV_INSTALL_FAILED)

        with self._lock:
            self.state.command = render_command(command)
            self.state.started_at = self._now()
            self.state.record(PHASE_STARTING, self.state.command)

        try:
            process = self._spawn(command, log_file=log_file)
        except Exception as error:
            return self._fail("could not spawn sglang: %s" % error, exitcodes.EXIT_START_FAILED)

        with self._lock:
            self._process = process
            self.state.service_pid = process.pid
            self.state.started_count += 1
            self.state.phase = PHASE_RUNNING

        # A service that dies immediately must not be reported as RUNNING.
        # sleep(0) was not enough: the child had not finished failing yet when
        # it was checked, so a dead service was reported as RUNNING with
        # exit_code 0.
        time.sleep(STARTUP_GRACE_SECONDS)
        if process.poll() is not None:
            code = getattr(process, "returncode", None)
            if code is None:
                code = process.poll()
            return self._fail("sglang exited immediately with code %s" % code,
                              exitcodes.EXIT_SERVICE_CRASHED)
        return StartResult(True, PHASE_RUNNING, self.state.command)

    def _spawn(self, command, log_file=""):
        """Start the service, capturing its output when a log file is given.

        The child's own output is the only diagnostic available when it fails
        during startup; sending it to DEVNULL made every early crash invisible
        and left the operator with nothing to read.
        """
        if self._runner is not None:
            return self._runner(command)
        if log_file:
            # Append, so repeated starts keep their history. The parent's copy
            # of the handle is closed below; the child keeps its own.
            output = open(log_file, "ab")  # noqa: SIM115 - closed after Popen
            try:
                return subprocess.Popen(  # noqa: S603 - the vector is allowlisted
                    command, stdout=output, stderr=subprocess.STDOUT,
                    start_new_session=True,
                )
            finally:
                output.close()
        return subprocess.Popen(  # noqa: S603 - the vector is built from an allowlist
            command,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
        )

    # --------------------------------------------------------------- stop

    def stop(self, timeout=60.0, force=False):
        """Stop SGLang and report the final exit code (§9)."""
        with self._lock:
            process = self._process
            if process is None or process.poll() is not None:
                self.state.stopped_at = self._now()
                self.state.service_pid = 0
                if self.state.phase != PHASE_FAILED:
                    self.state.record(PHASE_STOPPED, "no service was running")
                    self.state.exit_code = exitcodes.EXIT_OK
                return StopResult(self.state.phase, self.state.exit_code, False, "nothing to stop")
            self.state.record(PHASE_STOPPING, "graceful stop requested")
            self.state.stopped_count += 1

        if not force:
            self._terminate(process)
        deadline = self._now() + max(0.0, float(timeout))
        while self._now() < deadline:
            if process.poll() is not None:
                break
            time.sleep(0.05)

        timed_out = process.poll() is None
        if timed_out:
            self._kill(process)
            self._wait_briefly(process)

        exit_code = process.poll()
        if exit_code is None:
            exit_code = exitcodes.EXIT_STOP_TIMEOUT
        elif exit_code != 0 and not timed_out:
            # The service already crashed on its own: keep the crash code.
            exit_code = exitcodes.EXIT_SERVICE_CRASHED
        elif timed_out:
            exit_code = exitcodes.EXIT_STOP_TIMEOUT
        else:
            exit_code = exitcodes.EXIT_OK

        with self._lock:
            self._process = None
            self.state.service_pid = 0
            self.state.exit_code = exit_code
            self.state.stopped_at = self._now()
            # The failed flag is preserved: the control plane needs to know the
            # service was broken even though the process is gone now.
            if self.state.phase != PHASE_FAILED:
                self.state.record(PHASE_STOPPED, "stop completed with exit code %d" % exit_code)
            else:
                self.state.phase = PHASE_STOPPED
        return StopResult(self.state.phase, exit_code, timed_out,
                          "forced kill after the deadline" if timed_out else "")

    def _terminate(self, process):
        try:
            os.killpg(os.getpgid(process.pid), signal.SIGTERM)
        except (OSError, AttributeError):
            try:
                process.terminate()
            except OSError:
                pass

    def _kill(self, process):
        try:
            os.killpg(os.getpgid(process.pid), signal.SIGKILL)
        except (OSError, AttributeError):
            try:
                process.kill()
            except OSError:
                pass

    def _wait_briefly(self, process, timeout=5.0):
        deadline = self._now() + timeout
        while self._now() < deadline and process.poll() is None:
            time.sleep(0.05)

    # ------------------------------------------------------------ failures

    def _fail(self, message, exit_code):
        with self._lock:
            self.state.last_error = message
            self.state.exit_code = exit_code
            self.state.record(PHASE_FAILED, message)
        return StartResult(False, PHASE_FAILED, message)

    def note_environment(self, report):
        """Publish the detected environment on the status surface."""
        with self._lock:
            self.state.environment = report.as_dict()

    @staticmethod
    def _probe_http_health(port, timeout=2.0):
        url = "http://127.0.0.1:%d/health_generate" % int(port)
        try:
            with urlopen(url, timeout=timeout) as response:  # noqa: S310 - fixed localhost URL
                return 200 <= response.status < 300
        except (URLError, OSError, ValueError):
            return False


__all__ = [
    "PHASE_IDLE", "PHASE_INSTALLING", "PHASE_STARTING", "PHASE_RUNNING",
    "PHASE_STOPPING", "PHASE_STOPPED", "PHASE_FAILED", "PHASES",
    "BootstrapState", "StartResult", "StopResult", "SGLangService",
]
