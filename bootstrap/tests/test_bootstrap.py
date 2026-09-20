"""Unit tests for the bootstrap process: roles, profile, service and HTTP surface."""

import argparse
import contextlib
import io
import json
import os
import shutil
import sys
import tempfile
import threading
import time
import unittest
from http.client import HTTPConnection
from unittest.mock import patch

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from tai_talea_bootstrap import BOOTSTRAP_VERSION, exitcodes, __main__ as bootstrap_cli  # noqa: E402
from tai_talea_bootstrap.profile import (  # noqa: E402
    EnvironmentReport,
    ImageProfile,
    ProfileError,
    _matches,
    detect_environment,
    require_compatible,
    virtualenv_python,
)
from tai_talea_bootstrap.roles import (  # noqa: E402
    ConfigurationError,
    build_sglang_command,
    validate_extra_args,
)
from tai_talea_bootstrap.server import ENV_TOKEN, HEADER_TOKEN, BootstrapServer, serve  # noqa: E402
from tai_talea_bootstrap.service import (  # noqa: E402
    PHASE_FAILED,
    PHASE_IDLE,
    PHASE_RUNNING,
    PHASE_STOPPED,
    SGLangService,
)


# The shared secret every request in these tests must present.
TOKEN = "bootstrap-token-0123456789"


def profile(**overrides):
    values = {
        "os_version": "ubuntu-22.04",
        "cuda": "12.4",
        "python": "%d.%d" % (sys.version_info.major, sys.version_info.minor),
        "sglang": "0.4.x",
        "wheelhouse": "/opt/tai-talea/wheelhouse",
        "virtualenv": "/opt/tai-talea/venv",
        "bootstrap_version": BOOTSTRAP_VERSION,
    }
    values.update(overrides)
    return ImageProfile(**values)


class FakeProcess:
    """Minimal stand-in for a supervised child process."""

    def __init__(self, exit_code=None, pid=4321):
        self.pid = pid
        self._exit_code = exit_code
        self.terminated = False
        self.killed = False
        self.returncode = 0 if exit_code is None else exit_code

    def poll(self):
        if self.terminated or self.killed:
            return self._exit_code if self._exit_code is not None else 0
        return None

    def terminate(self):
        self.terminated = True

    def kill(self):
        self.killed = True


class DyingProcess(FakeProcess):
    """A child that is still alive when first checked and gone shortly after.

    `poll()` returns no exit code until `dies_after` seconds have elapsed. That
    ordering is what makes it a regression test for the startup grace window: a
    check that runs immediately sees a live process and wrongly calls it
    RUNNING.
    """

    def __init__(self, exit_code=1, dies_after=0.05):
        super().__init__(exit_code=exit_code)
        self._born = time.monotonic()
        self._dies_after = dies_after

    def poll(self):
        if time.monotonic() - self._born < self._dies_after:
            return None
        return self._exit_code


class FakeInstaller:
    def __init__(self, failure=None):
        self.failure = failure
        self.prepared = 0
        # Mirrors EnvironmentInstaller.python_executable.
        self.python_executable = "/opt/tai-talea/venv/bin/python"

    def prepare(self):
        self.prepared += 1
        if self.failure:
            raise self.failure
        return "/opt/tai-talea/venv/bin/python"


class RoleTests(unittest.TestCase):
    def test_prefill_and_decode_use_the_disaggregation_mode(self):
        prefill = build_sglang_command("prefill", "model-a", port=31000)
        decode = build_sglang_command("decode", "model-a", port=32000)
        self.assertIn("--disaggregation-mode", prefill)
        self.assertEqual(prefill[prefill.index("--disaggregation-mode") + 1], "prefill")
        self.assertEqual(decode[decode.index("--disaggregation-mode") + 1], "decode")
        self.assertEqual(prefill[0], "python3")
        self.assertEqual(prefill[1:3], ["-m", "sglang.launch_server"])

    def test_unknown_role_is_rejected(self):
        with self.assertRaises(ConfigurationError):
            build_sglang_command("router", "model-a")

    def test_model_id_is_required(self):
        with self.assertRaises(ConfigurationError):
            build_sglang_command("prefill", "   ")

    def test_port_range_is_enforced(self):
        for bad_port in (0, 70000, "abc"):
            with self.assertRaises(ConfigurationError):
                build_sglang_command("prefill", "model-a", port=bad_port)

    def test_control_characters_are_rejected(self):
        with self.assertRaises(ConfigurationError):
            build_sglang_command("prefill", "model-a\nrm -rf /")
        with self.assertRaises(ConfigurationError):
            build_sglang_command("prefill", "model-a", model_path="a\x00b")

    def test_extra_args_are_allowlisted(self):
        command = build_sglang_command(
            "decode", "model-a", extra_args=["--mem-fraction-static=0.8", "--tp-size=2"]
        )
        self.assertIn("--mem-fraction-static=0.8", command)

        for hostile in ("--model-path=/etc/passwd", "--host=evil", "--port=1", "; rm -rf /", "--api-key=x"):
            with self.assertRaises(ConfigurationError):
                validate_extra_args([hostile])


class ProfileTests(unittest.TestCase):
    def test_matching_environment_is_compatible(self):
        required = profile()
        report = detect_environment(required, sglang_version_probe=lambda _: "0.4.6")
        # The os and wheelhouse checks depend on the host, so only assert the
        # parts the probe controls.
        self.assertEqual(report.python, "%d.%d" % (sys.version_info.major, sys.version_info.minor))
        self.assertEqual(report.sglang, "0.4.6")

    def test_version_matching_rules(self):
        # A wildcard requirement accepts the whole minor line.
        self.assertTrue(_matches("0.4.x", "0.4.6"))
        self.assertTrue(_matches("0.4.x", "0.4.0"))
        self.assertFalse(_matches("0.4.x", "0.5.0"))
        # An exact requirement does not.
        self.assertTrue(_matches("0.4.6", "0.4.6"))
        self.assertFalse(_matches("0.4.6", "0.4.7"))
        # An empty requirement accepts anything.
        self.assertTrue(_matches("", "0.4.6"))

    def test_sglang_minor_mismatch_is_reported(self):
        required = profile(sglang="0.4.x")
        report = detect_environment(required, sglang_version_probe=lambda _: "0.5.0")
        self.assertIn("sglang=0.5.0 does not match profile 0.4.x", report.mismatches)
        self.assertFalse(report.compatible)

    def test_matching_sglang_is_not_reported_as_a_mismatch(self):
        report = detect_environment(profile(sglang="0.4.x"), sglang_version_probe=lambda _: "0.4.6")
        self.assertFalse(any("sglang" in mismatch for mismatch in report.mismatches))

    def test_missing_wheelhouse_is_reported(self):
        report = detect_environment(
            profile(wheelhouse="/definitely/not/here"), sglang_version_probe=lambda _: "0.4.6"
        )
        self.assertTrue(any("wheelhouse" in mismatch for mismatch in report.mismatches))

    def test_incomplete_profile_is_rejected(self):
        with self.assertRaises(ProfileError):
            profile(cuda="").validate()

    def test_require_compatible_raises_with_mismatches(self):
        required = profile(sglang="9.9.x")
        with self.assertRaises(ProfileError) as context:
            require_compatible(required, sglang_version_probe=lambda _: "0.4.6")
        self.assertTrue(context.exception.mismatches)


class DefaultProbeTests(unittest.TestCase):
    """The un-injected probe path is what production actually runs.

    Regression: ``_detect_sglang`` was handed an ``ImageProfile`` but read
    ``python_executable``, a field only ``EnvironmentInstaller`` has. Every unit
    test injected a probe, so ``check`` and ``serve`` crashed with exit code 70
    on a real container while the suite stayed green.
    """

    def test_virtualenv_python_resolves_the_managed_interpreter(self):
        venv = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, venv, True)
        os.makedirs(os.path.join(venv, "bin"))
        interpreter = os.path.join(venv, "bin", "python")
        with open(interpreter, "w", encoding="utf-8") as handle:
            handle.write("")
        self.assertEqual(virtualenv_python(profile(virtualenv=venv)), interpreter)
        self.assertEqual(virtualenv_python(profile(virtualenv="/definitely/not/here")), "")

    def test_detect_environment_without_a_probe_does_not_raise(self):
        report = detect_environment(profile(wheelhouse="/definitely/not/here",
                                            virtualenv="/definitely/not/here"))
        # No interpreter, so no version can be read: it must degrade to "" and be
        # reported as a mismatch rather than exploding.
        self.assertEqual(report.sglang, "")
        self.assertTrue(any("sglang" in mismatch for mismatch in report.mismatches))

    def test_check_subcommand_reports_incompatibility_not_a_crash(self):
        payload = {
            "os": "nowhere-1.0", "cuda": "12.4", "python": "3.11", "sglang": "0.4.x",
            "wheelhouse": "/definitely/not/here", "virtualenv": "/definitely/not/here",
        }
        path = os.path.join(tempfile.mkdtemp(), "profile.json")
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(payload, handle)
        buffer = io.StringIO()
        with contextlib.redirect_stdout(buffer):
            code = bootstrap_cli.main(["check", "--profile", path])
        self.assertEqual(code, exitcodes.EXIT_ENV_INCOMPATIBLE,
                         "check must report a mismatch (65), not an internal error (70)")
        report = json.loads(buffer.getvalue())
        self.assertFalse(report["compatible"])
        self.assertTrue(report["mismatches"])

    def test_check_subcommand_rejects_a_missing_profile_file(self):
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = bootstrap_cli.main(["check", "--profile", "/definitely/not/here.json"])
        self.assertEqual(code, exitcodes.EXIT_ENV_INCOMPATIBLE)
        self.assertIn("does not exist", stderr.getvalue())

    def test_version_banner_is_not_redundant(self):
        buffer = io.StringIO()
        with contextlib.redirect_stdout(buffer):
            with self.assertRaises(SystemExit):
                bootstrap_cli.main(["--version"])
        self.assertEqual(buffer.getvalue().strip(), BOOTSTRAP_VERSION)


class ServiceTests(unittest.TestCase):
    def make_service(self, process=None, installer=None, command_factory=None, health_probe=None):
        return SGLangService(
            profile(),
            installer=installer or FakeInstaller(),
            runner=lambda _command: process or FakeProcess(),
            command_factory=command_factory or build_sglang_command,
            health_probe=health_probe or (lambda _port: True),
        )

    def test_start_moves_to_running_and_reports_health(self):
        service = self.make_service()
        result = service.start("prefill", "model-a", port=31000)
        self.assertTrue(result.accepted)
        self.assertEqual(service.status()["phase"], PHASE_RUNNING)
        self.assertEqual(service.status()["role"], "prefill")
        health = service.health()
        self.assertEqual(health["status"], "ok")
        self.assertEqual(health["phase"], PHASE_RUNNING)
        self.assertTrue(any(entry["phase"] == "STARTING" for entry in service.status()["history"]))

    def test_start_rejects_a_second_service(self):
        service = self.make_service()
        self.assertTrue(service.start("prefill", "model-a").accepted)
        second = service.start("decode", "model-a")
        self.assertFalse(second.accepted)
        self.assertIn("already", second.detail)

    def test_concurrent_start_cannot_spawn_a_second_process(self):
        entered, release = threading.Event(), threading.Event()
        spawned = []

        def command(*args, **kwargs):
            entered.set()
            self.assertTrue(release.wait(5))
            return build_sglang_command(*args, **kwargs)

        service = self.make_service(command_factory=command)
        service._runner = lambda argv: spawned.append(argv) or FakeProcess()
        results = []
        worker = threading.Thread(target=lambda: results.append(service.start("prefill", "model-a", prepare_environment=False)))
        worker.start()
        try:
            self.assertTrue(entered.wait(5))
            self.assertFalse(service.start("decode", "model-a", prepare_environment=False).accepted)
        finally:
            release.set()
            worker.join(5)
        self.assertFalse(worker.is_alive())
        self.assertTrue(results[0].accepted)
        self.assertEqual(len(spawned), 1)

    def test_stop_during_preparation_prevents_a_late_start(self):
        entered, release = threading.Event(), threading.Event()
        installer = FakeInstaller()

        def prepare():
            entered.set()
            self.assertTrue(release.wait(5))
            return installer.python_executable

        installer.prepare = prepare
        service = self.make_service(installer=installer)
        results = []
        worker = threading.Thread(target=lambda: results.append(service.start("prefill", "model-a")))
        worker.start()
        try:
            self.assertTrue(entered.wait(5))
            self.assertEqual(service.stop(timeout=0).phase, PHASE_STOPPED)
        finally:
            release.set()
            worker.join(5)
        self.assertFalse(worker.is_alive())
        self.assertFalse(results[0].accepted)
        self.assertEqual(service.state.started_count, 0)
        self.assertEqual(service.status()["phase"], PHASE_STOPPED)

    def test_cancelled_preparation_error_does_not_overwrite_stop_or_next_start(self):
        entered, release = threading.Event(), threading.Event()
        installer = FakeInstaller()

        def prepare():
            entered.set()
            self.assertTrue(release.wait(5))
            raise RuntimeError("old preparation failed")

        installer.prepare = prepare
        service = self.make_service(installer=installer)
        results = []
        worker = threading.Thread(target=lambda: results.append(service.start("prefill", "old-model")))
        worker.start()
        try:
            self.assertTrue(entered.wait(5))
            service.stop(timeout=0)
            self.assertFalse(service.start("decode", "new-model", prepare_environment=False).accepted)
        finally:
            release.set()
            worker.join(5)
        self.assertFalse(results[0].accepted)
        self.assertEqual(service.status()["phase"], PHASE_STOPPED)
        self.assertEqual(service.state.last_error, "")
        self.assertTrue(service.start("decode", "new-model", prepare_environment=False).accepted)
        self.assertEqual(service.state.model_id, "new-model")
        self.assertEqual(service.state.last_error, "")

    def test_unowned_runner_never_signals_an_external_process_group(self):
        process = FakeProcess(pid=os.getpid())
        service = self.make_service(process=process)
        with patch("tai_talea_bootstrap.service.os.killpg", create=True) as signal_group:
            service._kill(process)
            self.assertTrue(process.killed)
            signal_group.assert_not_called()

    def test_reused_leader_pid_is_not_signalled(self):
        process = FakeProcess()
        process._talea_pgid = process.pid
        process._talea_start_time = 100
        service = self.make_service(process=process)
        with patch("tai_talea_bootstrap.service.Path.is_file", return_value=True), \
                patch("tai_talea_bootstrap.service._proc_stat", return_value=("S", process.pid, process.pid, 200)), \
                patch("tai_talea_bootstrap.service.os.killpg", create=True) as signal_group:
            service._kill(process)
            signal_group.assert_not_called()
            self.assertIsNone(process._talea_pgid)

    def test_stop_during_startup_grace_does_not_report_a_crash(self):
        entered, release = threading.Event(), threading.Event()
        service = self.make_service()
        results = []

        def grace(_seconds):
            entered.set()
            self.assertTrue(release.wait(5))

        with patch("tai_talea_bootstrap.service.time.sleep", side_effect=grace):
            worker = threading.Thread(target=lambda: results.append(service.start("prefill", "model-a")))
            worker.start()
            try:
                self.assertTrue(entered.wait(5))
                service.stop(timeout=0)
            finally:
                release.set()
                worker.join(5)
        self.assertFalse(results[0].accepted)
        self.assertEqual(service.status()["phase"], PHASE_STOPPED)
        self.assertEqual(service.state.last_error, "")

    def test_expected_sigterm_exit_is_successful(self):
        process = FakeProcess(exit_code=-15)
        service = self.make_service(process=process)
        self.assertTrue(service.start("prefill", "model-a").accepted)
        service._terminate = lambda child: child.terminate()
        result = service.stop(timeout=1)
        self.assertEqual(result.exit_code, exitcodes.EXIT_OK)

    def test_force_kills_immediately_and_retains_unreaped_process(self):
        process = FakeProcess()
        service = self.make_service(process=process)
        service.start("prefill", "model-a")
        service._kill = lambda child: child.kill()
        result = service.stop(timeout=60, force=True)
        self.assertTrue(process.killed)
        self.assertFalse(result.timed_out)
        process = FakeProcess()
        service._runner = lambda _command: process
        service.start("prefill", "model-a")
        service._kill = lambda child: None
        service._wait_briefly = lambda child: None
        result = service.stop(timeout=0, force=True)
        self.assertEqual(result.phase, PHASE_FAILED)
        self.assertEqual(service.status()["service_pid"], process.pid)
        self.assertFalse(service.start("decode", "model-a").accepted)

    def test_invalid_role_fails_with_bad_request(self):
        service = self.make_service()
        result = service.start("router", "model-a")
        self.assertFalse(result.accepted)
        self.assertEqual(service.status()["phase"], PHASE_FAILED)
        self.assertEqual(service.status()["exit_code"], exitcodes.EXIT_BAD_REQUEST)

    def test_environment_failure_maps_to_its_exit_code(self):
        installer = FakeInstaller(failure=RuntimeError("wheelhouse is empty"))
        service = self.make_service(installer=installer)
        result = service.start("prefill", "model-a")
        self.assertFalse(result.accepted)
        self.assertEqual(service.status()["exit_code"], exitcodes.EXIT_ENV_INSTALL_FAILED)

    def test_sglang_command_does_not_carry_a_log_file_flag(self):
        """SGLang 0.5.x rejects --log-file inside argparse.

        Passing it made the child exit before producing any output, and the
        bootstrap went on reporting RUNNING. The service's output is captured
        by the bootstrap itself, not by an SGLang flag.
        """
        command = build_sglang_command(
            "prefill", "model-a",
            # The allowlist matches whole tokens, so values ride along with an
            # equals sign - exactly how the control plane sends them.
            extra_args=["--tp-size=1",
                        "--disaggregation-transfer-backend=mooncake_tcp",
                        "--disaggregation-bootstrap-port=5757"])
        self.assertIn("--tp-size=1", command, "extra args must still reach the command")
        self.assertIn("--disaggregation-transfer-backend=mooncake_tcp", command)
        self.assertIn("--disaggregation-bootstrap-port=5757", command)
        self.assertNotIn("--log-file", command)

    def test_spawn_captures_the_child_output_into_the_log_file(self):
        directory = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, directory, True)
        path = os.path.join(directory, "sglang.log")
        # A real Popen is required here: a runner stub would bypass the
        # redirection this test is about.
        service = SGLangService(
            profile(),
            installer=FakeInstaller(),
            runner=None,
            command_factory=build_sglang_command,
            health_probe=lambda _port: True,
        )
        process = service._spawn(
            [sys.executable, "-c", "print('bootstrap-captured-output')"],
            log_file=path,
        )
        self.assertTrue(process.wait(timeout=20) == 0)
        with open(path, encoding="utf-8") as handle:
            self.assertIn("bootstrap-captured-output", handle.read())

    def test_stop_reports_a_clean_exit_code(self):
        service = self.make_service()
        service.start("decode", "model-a")
        result = service.stop(timeout=1.0)
        self.assertEqual(result.exit_code, exitcodes.EXIT_OK)
        self.assertFalse(result.timed_out)
        self.assertEqual(service.status()["phase"], PHASE_STOPPED)

    def test_child_inherits_the_managed_virtualenv_environment(self):
        directory = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, directory, True)
        installer = FakeInstaller()
        installer.python_executable = os.path.join(directory, "bin", "python")
        service = SGLangService(profile(virtualenv=directory), installer=installer)
        # Exercise both real Popen branches. A Python launched by absolute path
        # still needs the venv's bin directory when it invokes tools like ninja.
        code = (
            "import os, sys; "
            "assert os.environ['VIRTUAL_ENV'] == sys.argv[1]; "
            "assert os.environ['PATH'].split(os.pathsep)[0] == sys.argv[2]"
        )
        for log_file in ("", os.path.join(directory, "child.log")):
            with self.subTest(log_file=log_file):
                process = service._spawn(
                    [sys.executable, "-c", code, directory,
                     os.path.dirname(installer.python_executable)], log_file=log_file)
                self.assertEqual(process.wait(timeout=20), 0)

    def test_late_exit_is_reported_by_status_and_health_once(self):
        for first_query in ("status", "health"):
            with self.subTest(first_query=first_query):
                process = FakeProcess(exit_code=3)
                service = self.make_service(process=process)
                self.assertTrue(service.start("prefill", "model-a").accepted)
                process.terminated = True
                self.assertEqual(getattr(service, first_query)()["phase"], PHASE_FAILED)
                status = service.status()
                self.assertEqual(status["exit_code"], 3)
                self.assertGreater(status["stopped_at"], 0)
                self.assertEqual(service.health()["status"], "failed")
                self.assertIn("code 3", status["last_error"])
                failures = [e for e in service.status()["history"] if e["phase"] == PHASE_FAILED]
                self.assertEqual(len(failures), 1)

    def test_start_can_retry_a_late_exit_without_a_prior_status_query(self):
        process = FakeProcess(exit_code=3)
        service = self.make_service(process=process)
        self.assertTrue(service.start("prefill", "model-a").accepted)
        process.terminated = True
        replacement = FakeProcess(pid=4322)
        service._runner = lambda _command: replacement
        self.assertTrue(service.start("prefill", "model-a").accepted)
        self.assertEqual(service.status()["service_pid"], replacement.pid)
        self.assertEqual(service.status()["last_error"], "")

    def test_stop_timeout_is_reported(self):
        stubborn = FakeProcess()
        stubborn.terminate = lambda: None  # ignores SIGTERM
        service = SGLangService(
            profile(),
            installer=FakeInstaller(),
            runner=lambda _command: stubborn,
            health_probe=lambda _port: True,
            now=lambda: time.time(),
        )
        service.start("prefill", "model-a")
        service._terminate = lambda process: None  # simulate a process that ignores SIGTERM
        result = service.stop(timeout=0.05)
        self.assertTrue(result.timed_out)
        self.assertEqual(result.exit_code, exitcodes.EXIT_STOP_TIMEOUT)
        self.assertTrue(stubborn.killed)

    def test_start_without_preparing_still_uses_the_managed_interpreter(self):
        """The interpreter comes from the profile, not from `python3`.

        Regression: with prepare_environment=False the command was built with a
        bare "python3", so a real start launched the system interpreter, which
        has no sglang, and the child died before it ever listened.
        """
        installer = FakeInstaller()
        service = self.make_service(installer=installer)
        result = service.start("prefill", "model-a", prepare_environment=False)
        self.assertTrue(result.accepted)
        self.assertEqual(installer.prepared, 0, "no preparation was requested")
        # The command an operator sees in the history must name the venv.
        started = [e for e in service.status()["history"] if e["phase"] == "STARTING"]
        self.assertEqual(len(started), 1)
        self.assertIn("/opt/tai-talea/venv/bin/python", started[0]["detail"])
        self.assertNotIn("python3 ", started[0]["detail"])

    def test_a_process_that_dies_just_after_spawn_is_not_reported_running(self):
        """The immediate-exit check needs to outlive the spawn by a moment.

        A real `python3 -m <missing module>` is still alive at the instant of
        the first poll and gone a few milliseconds later, so a check that runs
        immediately reported RUNNING with exit_code 0. This fake reproduces
        that ordering instead of reporting the exit code on the first call.
        """
        service = self.make_service(process=DyingProcess(exit_code=1, dies_after=0.05))
        result = service.start("prefill", "model-a")
        self.assertFalse(result.accepted, "a service that died must not be accepted")
        self.assertEqual(service.status()["phase"], PHASE_FAILED)
        self.assertEqual(service.status()["exit_code"], exitcodes.EXIT_SERVICE_CRASHED)

    def test_immediate_crash_is_reported(self):
        dead = FakeProcess(exit_code=1)
        dead.terminated = True
        service = self.make_service(process=dead)
        result = service.start("prefill", "model-a")
        self.assertFalse(result.accepted)
        self.assertEqual(service.status()["exit_code"], exitcodes.EXIT_SERVICE_CRASHED)

    def test_health_reports_degraded_when_sglang_does_not_answer(self):
        service = self.make_service(health_probe=lambda _port: False)
        service.start("prefill", "model-a")
        self.assertEqual(service.health()["status"], "degraded")

    def test_health_before_start_is_idle(self):
        service = self.make_service()
        self.assertEqual(service.status()["phase"], PHASE_IDLE)
        self.assertEqual(service.health()["status"], "ok")

    def test_note_environment_is_published(self):
        service = self.make_service()
        service.note_environment(EnvironmentReport(os="ubuntu-22.04", cuda="12.4"))
        self.assertEqual(service.status()["environment"]["cuda"], "12.4")


class HttpSurfaceTests(unittest.TestCase):
    def setUp(self):
        self.process = FakeProcess()
        self.service = SGLangService(
            profile(),
            installer=FakeInstaller(),
            runner=lambda _command: self.process,
            health_probe=lambda _port: True,
        )
        self.server = BootstrapServer(("127.0.0.1", 0), self.service, TOKEN)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def request(self, method, path, payload=None, token=TOKEN):
        connection = HTTPConnection("127.0.0.1", self.port, timeout=5)
        body = json.dumps(payload).encode("utf-8") if payload is not None else None
        headers = {"Content-Type": "application/json"} if body else {}
        if token is not None:
            headers[HEADER_TOKEN] = token
        connection.request(method, path, body=body, headers=headers)
        response = connection.getresponse()
        raw = response.read()
        connection.close()
        return response.status, json.loads(raw.decode("utf-8"))

    def test_health_reports_the_bootstrap_version(self):
        status, payload = self.request("GET", "/bootstrap/health")
        self.assertEqual(status, 200)
        self.assertEqual(payload["version"], BOOTSTRAP_VERSION)

    def test_status_starts_idle(self):
        status, payload = self.request("GET", "/bootstrap/status")
        self.assertEqual(status, 200)
        self.assertEqual(payload["phase"], PHASE_IDLE)

    def test_nonfinite_stop_timeout_is_rejected_without_stopping(self):
        self.service.start("prefill", "model-a")
        for timeout in ("NaN", "Infinity", "-Infinity"):
            with self.subTest(timeout=timeout):
                status, _ = self.request("POST", "/bootstrap/stop", {"timeout": timeout})
                self.assertEqual(status, 400)
                self.assertFalse(self.process.terminated)
                self.assertFalse(self.process.killed)

    def test_start_then_status_then_stop(self):
        status, payload = self.request("POST", "/bootstrap/start", {
            "role": "prefill", "model_id": "model-a", "port": 31000, "prepare_environment": False,
        })
        self.assertEqual(status, 202)
        self.assertTrue(payload["accepted"])

        status, payload = self.request("GET", "/bootstrap/status")
        self.assertEqual(payload["phase"], PHASE_RUNNING)
        self.assertEqual(payload["role"], "prefill")

        status, payload = self.request("POST", "/bootstrap/stop", {"timeout": 1})
        self.assertEqual(status, 200)
        self.assertEqual(payload["exit_code"], exitcodes.EXIT_OK)

    def test_start_with_a_bad_role_is_rejected(self):
        status, payload = self.request("POST", "/bootstrap/start", {
            "role": "router", "model_id": "model-a", "prepare_environment": False,
        })
        self.assertEqual(status, 422)
        self.assertFalse(payload["accepted"])

    def test_hostile_extra_args_are_rejected(self):
        status, payload = self.request("POST", "/bootstrap/start", {
            "role": "prefill", "model_id": "model-a", "prepare_environment": False,
            "extra_args": ["--model-path=/etc/shadow"],
        })
        self.assertEqual(status, 422)
        self.assertFalse(payload["accepted"])

    def test_invalid_json_is_rejected(self):
        connection = HTTPConnection("127.0.0.1", self.port, timeout=5)
        connection.request("POST", "/bootstrap/start", body=b"not json",
                           headers={"Content-Type": "application/json", HEADER_TOKEN: TOKEN})
        response = connection.getresponse()
        self.assertEqual(response.status, 400)
        response.read()
        connection.close()

    def test_unknown_path_is_not_found(self):
        status, _ = self.request("GET", "/bootstrap/exec")
        self.assertEqual(status, 404)


class ServerShutdownTests(unittest.TestCase):
    def test_external_stop_event_terminates_http_server(self):
        stop, ready = threading.Event(), threading.Event()
        servers, results = [], []

        def create(*args, **kwargs):
            server = BootstrapServer(*args, **kwargs)
            servers.append(server)
            ready.set()
            return server

        with patch("tai_talea_bootstrap.server.BootstrapServer", side_effect=create):
            worker = threading.Thread(target=lambda: results.append(serve(
                SGLangService(profile()), port=0, token=TOKEN, stop_event=stop)), daemon=True)
            worker.start()
            try:
                self.assertTrue(ready.wait(5))
                stop.set()
                worker.join(3)
                self.assertFalse(worker.is_alive())
                self.assertEqual(results, [exitcodes.EXIT_OK])
            finally:
                stop.set()
                if worker.is_alive() and servers:
                    servers[0].shutdown()
                    worker.join(3)


class BootstrapAuthTests(unittest.TestCase):
    """The bootstrap interface has no unauthenticated mode.

    Partner platforms routinely publish container ports to the internet, so the
    shared secret - not the network boundary - is what protects /bootstrap/start
    and /bootstrap/stop.
    """

    def setUp(self):
        self.process = FakeProcess()
        # Recording the launch commands is what proves a rejected request never
        # reached the service: FakeProcess.pid is a constant, not a real state.
        self.launches = []

        def runner(command):
            self.launches.append(command)
            return self.process

        self.service = SGLangService(
            profile(),
            installer=FakeInstaller(),
            runner=runner,
            health_probe=lambda _port: True,
        )
        self.server = BootstrapServer(("127.0.0.1", 0), self.service, TOKEN)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def call(self, method, path, payload=None, token=None):
        connection = HTTPConnection("127.0.0.1", self.port, timeout=5)
        body = json.dumps(payload).encode("utf-8") if payload is not None else None
        headers = {"Content-Type": "application/json"} if body else {}
        if token is not None:
            headers[HEADER_TOKEN] = token
        connection.request(method, path, body=body, headers=headers)
        response = connection.getresponse()
        raw = response.read()
        connection.close()
        return response.status, json.loads(raw.decode("utf-8"))

    def test_every_endpoint_refuses_a_missing_or_wrong_secret(self):
        endpoints = (
            ("GET", "/bootstrap/health", None),
            ("GET", "/bootstrap/status", None),
            ("POST", "/bootstrap/start", {"role": "prefill", "model_id": "model-a"}),
            ("POST", "/bootstrap/stop", {"timeout": 1}),
        )
        for method, path, payload in endpoints:
            for token in (None, "", "wrong-token", TOKEN + "x", TOKEN[:-1]):
                status, body = self.call(method, path, payload, token=token)
                self.assertEqual(
                    status, 401, "%s %s accepted token=%r" % (method, path, token))
                self.assertEqual(body["error"], "unauthorized")

    def test_a_rejected_start_leaves_the_service_untouched(self):
        status, _ = self.call("POST", "/bootstrap/start",
                              {"role": "prefill", "model_id": "model-a"}, token="wrong-token")
        self.assertEqual(status, 401)
        # Nothing may have happened: no SGLang process was launched and the
        # service is still idle.
        self.assertEqual(self.launches, [])
        self.assertEqual(self.service.status()["phase"], PHASE_IDLE)

    def test_the_correct_secret_is_accepted(self):
        status, payload = self.call("GET", "/bootstrap/health", token=TOKEN)
        self.assertEqual(status, 200)
        self.assertEqual(payload["status"], "ok")

    def test_authorization_precedes_routing(self):
        # An unknown path must not reveal itself to an unauthenticated caller.
        status, _ = self.call("GET", "/bootstrap/exec")
        self.assertEqual(status, 401)
        status, _ = self.call("GET", "/bootstrap/exec", token=TOKEN)
        self.assertEqual(status, 404)

    def test_server_refuses_to_start_without_a_secret(self):
        for token in ("", "   ", None):
            with self.assertRaises(ValueError):
                BootstrapServer(("127.0.0.1", 0), self.service, token)


class BootstrapSecretSourceTests(unittest.TestCase):
    """Where the secret comes from, and the fact that there is always a source."""

    def test_token_file_wins_over_the_environment(self):
        directory = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, directory, True)
        path = os.path.join(directory, "bootstrap.token")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("from-the-file\n")

        saved = os.environ.get(ENV_TOKEN)
        os.environ[ENV_TOKEN] = "from-the-environment"
        try:
            args = argparse.Namespace(token_file=path)
            self.assertEqual(bootstrap_cli.resolve_token(args), "from-the-file")
            args.token_file = ""
            self.assertEqual(bootstrap_cli.resolve_token(args), "from-the-environment")
            args.token_file = "/definitely/not/here"
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(bootstrap_cli.resolve_token(args), "")
        finally:
            if saved is None:
                os.environ.pop(ENV_TOKEN, None)
            else:
                os.environ[ENV_TOKEN] = saved

    def test_serve_fails_closed_without_a_secret(self):
        saved = os.environ.pop(ENV_TOKEN, None)
        try:
            stderr = io.StringIO()
            with contextlib.redirect_stderr(stderr):
                code = bootstrap_cli.main(["serve", "--profile", "/definitely/not/here.json"])
            self.assertEqual(code, exitcodes.EXIT_AUTH_NOT_CONFIGURED)
            self.assertIn("auth_not_configured", stderr.getvalue())
        finally:
            if saved is not None:
                os.environ[ENV_TOKEN] = saved

    def test_status_command_authenticates(self):
        server = BootstrapServer(("127.0.0.1", 0), _idle_service(), TOKEN)
        port = server.server_address[1]
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)

        saved = os.environ.get(ENV_TOKEN)
        try:
            os.environ[ENV_TOKEN] = TOKEN
            stdout = io.StringIO()
            with contextlib.redirect_stdout(stdout):
                code = bootstrap_cli.main(["status", "--host", "127.0.0.1", "--port", str(port)])
            self.assertEqual(code, exitcodes.EXIT_OK)
            self.assertEqual(json.loads(stdout.getvalue())["phase"], PHASE_IDLE)

            os.environ[ENV_TOKEN] = "wrong-token"
            with contextlib.redirect_stderr(io.StringIO()):
                code = bootstrap_cli.main(["status", "--host", "127.0.0.1", "--port", str(port)])
            self.assertEqual(code, exitcodes.EXIT_AUTH_NOT_CONFIGURED)

            os.environ.pop(ENV_TOKEN, None)
            with contextlib.redirect_stderr(io.StringIO()):
                code = bootstrap_cli.main(["status", "--host", "127.0.0.1", "--port", str(port)])
            self.assertEqual(code, exitcodes.EXIT_AUTH_NOT_CONFIGURED)
        finally:
            if saved is None:
                os.environ.pop(ENV_TOKEN, None)
            else:
                os.environ[ENV_TOKEN] = saved


def _idle_service():
    return SGLangService(
        profile(),
        installer=FakeInstaller(),
        runner=lambda _command: FakeProcess(),
        health_probe=lambda _port: True,
    )


class ServeCommandTests(unittest.TestCase):
    """`serve` is only reachable through the CLI entry point.

    Every other suite builds BootstrapServer / SGLangService directly, so a
    broken command_serve stays invisible to them. That is exactly how a
    regression shipped: commit 454f1c3 rewrote command_serve to add the shared
    secret and passed the whole argparse Namespace to load_profile, so every
    real `serve` start died with {"error": "internal"} and exit code 70 while
    `check` kept passing.
    """

    def setUp(self):
        directory = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, directory, True)
        self.path = os.path.join(directory, "profile.json")
        # Deliberately incompatible, so serve returns before it would listen.
        payload = {
            "os": "nowhere-1.0", "cuda": "12.4", "python": "3.10", "sglang": "0.3.0",
            "wheelhouse": "/definitely/not/here", "virtualenv": "/definitely/not/here",
        }
        with open(self.path, "w", encoding="utf-8") as handle:
            json.dump(payload, handle)
        self.addCleanup(os.environ.pop, ENV_TOKEN, None)
        os.environ[ENV_TOKEN] = "s" * 32

    def run_serve(self):
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = bootstrap_cli.main(["serve", "--profile", self.path])
        return code, stdout.getvalue(), stderr.getvalue()

    def test_serve_reads_the_profile_file_not_the_argument_namespace(self):
        code, _, stderr = self.run_serve()
        self.assertEqual(
            code, exitcodes.EXIT_ENV_INCOMPATIBLE,
            "serve must report an incompatible environment (65), not crash with an "
            "internal error (70): %s" % stderr)
        self.assertIn("environment_incompatible", stderr)

    def test_serve_refuses_to_start_without_a_shared_secret(self):
        os.environ.pop(ENV_TOKEN, None)
        code, _, stderr = self.run_serve()
        self.assertEqual(code, exitcodes.EXIT_AUTH_NOT_CONFIGURED)
        self.assertIn("auth_not_configured", stderr)

    def test_serve_rejects_a_missing_profile_file(self):
        missing = os.path.join(os.path.dirname(self.path), "nope.json")
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = bootstrap_cli.main(["serve", "--profile", missing])
        self.assertEqual(code, exitcodes.EXIT_ENV_INCOMPATIBLE)
        self.assertIn("profile_invalid", stderr.getvalue())

    def test_shutdown_cleans_failed_service_and_preserves_failure_exit(self):
        process = FakeProcess()
        service = SGLangService(profile(), installer=FakeInstaller(), runner=lambda _command: process)

        def failed_serve(*_args, **_kwargs):
            service._process = process
            service.state.service_pid = process.pid
            service.state.phase = PHASE_FAILED
            service.state.last_error = "leader failed with remaining workers"
            service.state.exit_code = 0

        with patch.object(bootstrap_cli, "detect_environment", return_value=EnvironmentReport()), \
                patch.object(bootstrap_cli, "SGLangService", return_value=service), \
                patch.object(bootstrap_cli, "serve", side_effect=failed_serve), \
                patch("signal.signal"):
            code, _, _ = self.run_serve()
        self.assertTrue(process.terminated)
        self.assertEqual(service.state.phase, PHASE_STOPPED)
        self.assertEqual(code, exitcodes.EXIT_START_FAILED)


class ExitCodeTests(unittest.TestCase):
    def test_every_code_is_documented(self):
        for code in (
            exitcodes.EXIT_OK,
            exitcodes.EXIT_BAD_REQUEST,
            exitcodes.EXIT_ENV_INCOMPATIBLE,
            exitcodes.EXIT_ENV_INSTALL_FAILED,
            exitcodes.EXIT_START_FAILED,
            exitcodes.EXIT_SERVICE_CRASHED,
            exitcodes.EXIT_STOP_TIMEOUT,
            exitcodes.EXIT_INTERNAL,
            exitcodes.EXIT_AUTH_NOT_CONFIGURED,
        ):
            self.assertNotEqual(exitcodes.describe(code), "unknown exit code")

    def test_describe_handles_unknown_codes(self):
        self.assertEqual(exitcodes.describe(200), "unknown exit code")


if __name__ == "__main__":
    unittest.main(verbosity=2)
