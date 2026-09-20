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
from pathlib import Path
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


def _proc_stat(pid):
    """Linux process identity; comm can itself contain spaces and parentheses."""
    fields = Path("/proc/%s/stat" % pid).read_text().rsplit(")", 1)[1].split()
    return fields[0], int(fields[2]), int(fields[3]), int(fields[19])


def _own_process_group(process):
    # Popen(start_new_session=True) made this child the session/group leader.
    # Never infer group ownership from an arbitrary injected runner or a PID
    # lookup after the leader has exited (its PID may have been reused).
    if os.name == "posix":
        process._talea_pgid = process.pid
        try:
            process._talea_start_time = _proc_stat(process.pid)[3]
        except (OSError, ValueError, IndexError):
            process._talea_start_time = None
    return process


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
        self._start_lock = threading.Lock()
        self._stop_lock = threading.Lock()
        self._generation = 0
        self._stopping = False
        self._process = None
        self.state = BootstrapState(environment=profile.as_dict())

    # ------------------------------------------------------------- queries

    def status(self):
        with self._lock:
            self._refresh_process()
            return self.state.as_dict()

    def health(self):
        """Health surface: the bootstrap is up, and whether SGLang answers."""
        with self._lock:
            self._refresh_process()
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

    def _refresh_process(self):
        """Observe exits after the initial startup grace period, under the lock."""
        if self.state.phase != PHASE_RUNNING or self._process is None:
            return
        code = self._process.poll()
        if code is None:
            return
        self.state.exit_code = code
        self.state.stopped_at = self._now()
        self.state.last_error = "sglang exited unexpectedly with code %s" % code
        self.state.record(PHASE_FAILED, self.state.last_error)

    @property
    def _port(self):
        return getattr(self, "_bound_port", 31000)

    # -------------------------------------------------------------- start

    def start(self, role, model_id, model_path="", port=31000, extra_args=(), log_file="",
              prepare_environment=True):
        """Prepare the environment and start SGLang for one role."""
        if not self._start_lock.acquire(blocking=False):
            return StartResult(False, self.state.phase, "a service is already starting")
        try:
            return self._start(role, model_id, model_path, port, extra_args, log_file,
                               prepare_environment)
        finally:
            self._start_lock.release()

    def _start(self, role, model_id, model_path, port, extra_args, log_file,
               prepare_environment):
        with self._lock:
            self._refresh_process()
            alive = self._process is not None and self._process_alive(self._process)
            if (self._stopping or alive or self.state.phase in
                    (PHASE_RUNNING, PHASE_STARTING, PHASE_INSTALLING, PHASE_STOPPING)):
                return StartResult(False, self.state.phase,
                                   "a service is already %s" % self.state.phase)
            self._generation += 1
            generation = self._generation
            self.state.phase = PHASE_INSTALLING if prepare_environment else PHASE_STARTING
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
                    if generation != self._generation:
                        return StartResult(False, self.state.phase, "start was cancelled by stop")
                    self.state.record(PHASE_INSTALLING, "preparing the offline virtualenv")
                python_executable = self.installer.prepare()
            command = self._command_factory(
                role, model_id, model_path=model_path, port=port,
                python_executable=python_executable, extra_args=extra_args,
            )
        except ConfigurationError as error:
            return self._start_failure(generation, str(error), exitcodes.EXIT_BAD_REQUEST)
        except Exception as error:  # environment preparation failures
            return self._start_failure(generation, "environment preparation failed: %s" % error,
                                       exitcodes.EXIT_ENV_INSTALL_FAILED)

        with self._lock:
            if generation != self._generation:
                return StartResult(False, self.state.phase, "start was cancelled by stop")
            self.state.command = render_command(command)
            self.state.started_at = self._now()
            self.state.record(PHASE_STARTING, self.state.command)

            # Keep process publication atomic with respect to stop. Otherwise a
            # stop between spawn and assignment can report success yet orphan it.
            try:
                process = self._spawn(command, log_file=log_file)
            except Exception as error:
                return self._fail("could not spawn sglang: %s" % error, exitcodes.EXIT_START_FAILED)
            self._process = process
            self.state.service_pid = process.pid
            self.state.started_count += 1
            self.state.phase = PHASE_RUNNING

        # A service that dies immediately must not be reported as RUNNING.
        # sleep(0) was not enough: the child had not finished failing yet when
        # it was checked, so a dead service was reported as RUNNING with
        # exit_code 0.
        time.sleep(STARTUP_GRACE_SECONDS)
        with self._lock:
            if generation != self._generation:
                return StartResult(False, self.state.phase, "start was cancelled by stop")
            if process.poll() is not None:
                code = getattr(process, "returncode", None)
                if code is None:
                    code = process.poll()
                return self._fail("sglang exited immediately with code %s" % code,
                                  exitcodes.EXIT_SERVICE_CRASHED)
            return StartResult(True, PHASE_RUNNING, self.state.command)

    def _start_failure(self, generation, message, code):
        with self._lock:
            if generation != self._generation:
                return StartResult(False, self.state.phase, "start was cancelled by stop")
            return self._fail(message, code)

    def _spawn(self, command, log_file=""):
        """Start the service, capturing its output when a log file is given.

        The child's own output is the only diagnostic available when it fails
        during startup; sending it to DEVNULL made every early crash invisible
        and left the operator with nothing to read.
        """
        if self._runner is not None:
            return self._runner(command)
        environment = os.environ.copy()
        environment["VIRTUAL_ENV"] = self.profile.virtualenv
        environment["PATH"] = (
            os.path.dirname(self.installer.python_executable)
            + os.pathsep + environment.get("PATH", "")
        )
        if log_file:
            # Append, so repeated starts keep their history. The parent's copy
            # of the handle is closed below; the child keeps its own.
            output = open(log_file, "ab")  # noqa: SIM115 - closed after Popen
            try:
                return _own_process_group(subprocess.Popen(  # noqa: S603 - the vector is allowlisted
                    command, stdout=output, stderr=subprocess.STDOUT,
                    start_new_session=True, env=environment,
                ))
            finally:
                output.close()
        return _own_process_group(subprocess.Popen(  # noqa: S603 - the vector is built from an allowlist
            command,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True, env=environment,
        ))

    # --------------------------------------------------------------- stop

    def stop(self, timeout=60.0, force=False):
        """Stop SGLang and report the final exit code (§9)."""
        with self._stop_lock:
            with self._lock:
                self._generation += 1
                self._stopping = True
            try:
                return self._stop(timeout, force)
            finally:
                with self._lock:
                    self._stopping = False

    def _stop(self, timeout, force):
        with self._lock:
            process = self._process
            if process is None or not self._process_alive(process):
                self._process = None
                self.state.stopped_at = self._now()
                self.state.service_pid = 0
                if self.state.phase != PHASE_FAILED:
                    self.state.exit_code = exitcodes.EXIT_OK
                self.state.record(PHASE_STOPPED, "no service was running")
                return StopResult(self.state.phase, self.state.exit_code, False, "nothing to stop")
            self.state.record(PHASE_STOPPING, "graceful stop requested")
            self.state.stopped_count += 1

        if force:
            self._kill(process)
        else:
            self._terminate(process)
        deadline = self._now() + max(0.0, float(timeout))
        while self._now() < deadline:
            if not self._process_alive(process):
                break
            time.sleep(0.05)

        timed_out = self._process_alive(process)
        if timed_out:
            self._kill(process)
            self._wait_briefly(process)

        exit_code = process.poll()
        if self._process_alive(process):
            with self._lock:
                self.state.exit_code = exitcodes.EXIT_STOP_TIMEOUT
                self.state.last_error = "sglang process group is still alive after forced termination"
                self.state.record(PHASE_FAILED, self.state.last_error)
            return StopResult(PHASE_FAILED, exitcodes.EXIT_STOP_TIMEOUT, True,
                              self.state.last_error)
        expected_signals = (-signal.SIGTERM,)
        if force:
            expected_signals += (-getattr(signal, "SIGKILL", 9),)
        if exit_code != 0 and exit_code not in expected_signals and not timed_out:
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
                          "forced kill after the deadline" if timed_out else
                          ("forced stop requested" if force else ""))

    def _terminate(self, process):
        self._signal(process, signal.SIGTERM, process.terminate)

    def _kill(self, process):
        self._signal(process, getattr(signal, "SIGKILL", 9), process.kill)

    def _signal(self, process, signum, fallback):
        pgid = getattr(process, "_talea_pgid", None)
        if pgid is not None:
            if self._group_alive(process):
                try:
                    os.killpg(pgid, signum)
                except OSError:
                    pass
            return
        # Custom runners are not known to own a process group. They must never
        # cause us to signal the bootstrap's or another application's group.
        try:
            if process.poll() is None:
                fallback()
        except OSError:
            pass

    def _group_alive(self, process):
        pgid = getattr(process, "_talea_pgid", None)
        if pgid is None:
            return False
        if Path("/proc/self/stat").is_file():
            try:
                # A different leader at this PID means the old group is gone.
                identity = _proc_stat(pgid)
                original = getattr(process, "_talea_start_time", None)
                if original is not None and identity[3] != original:
                    process._talea_pgid = None
                    return False
            except FileNotFoundError:
                pass  # The leader exited; its children can still own the group.
            except (OSError, ValueError, IndexError):
                return True
            try:
                for entry in Path("/proc").iterdir():
                    if not entry.name.isdigit():
                        continue
                    try:
                        state, group, session, _ = _proc_stat(entry.name)
                    except FileNotFoundError:
                        continue
                    except (OSError, ValueError, IndexError):
                        return True
                    if group == pgid and session == pgid and state not in ("Z", "X"):
                        return True
            except OSError:
                return True
            # Zombies hold no GPU memory; container PID 1 may delay reaping them.
            process._talea_pgid = None
            return False
        try:
            os.killpg(pgid, 0)
            return True
        except ProcessLookupError:
            process._talea_pgid = None
            return False
        except OSError:
            return True

    def _process_alive(self, process):
        return process.poll() is None or self._group_alive(process)

    def _wait_briefly(self, process, timeout=5.0):
        deadline = self._now() + timeout
        while self._now() < deadline and self._process_alive(process):
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
