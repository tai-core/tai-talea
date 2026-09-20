"""Real Linux process supervision, independent of CUDA and SGLang packages."""

import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from tai_talea_bootstrap import exitcodes
from tai_talea_bootstrap.profile import ImageProfile
from tai_talea_bootstrap.service import SGLangService, _proc_stat


@unittest.skipUnless(sys.platform.startswith("linux"), "requires Linux process groups and /proc")
class ProcessGroupTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.groups = []
        self.addCleanup(self.cleanup_groups)

    def cleanup_groups(self):
        for process in self.groups:
            if getattr(process, "_talea_pgid", process.pid) is None:
                process.wait(timeout=5)
                continue
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait(timeout=5)

    def wait_for(self, predicate, message):
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if predicate():
                return
            time.sleep(0.01)
        self.fail(message)

    @staticmethod
    def alive(pid):
        try:
            return _proc_stat(pid)[0] not in ("Z", "X")
        except FileNotFoundError:
            return False

    def create_service(self, ignore_term=False):
        child_ready = self.root / "child-ready"
        child_pid = self.root / "child-pid"
        exit_parent = self.root / "exit-parent"
        child_code = (
            "import signal,time; from pathlib import Path; "
            + ("signal.signal(signal.SIGTERM,signal.SIG_IGN); " if ignore_term else "")
            + "Path(%r).write_text('ready'); time.sleep(120)" % str(child_ready)
        )
        parent_code = "\n".join([
            "import os,subprocess,sys,time",
            "from pathlib import Path",
            "child=subprocess.Popen([sys.executable,'-c',%r])" % child_code,
            "while not Path(%r).exists(): time.sleep(.01)" % str(child_ready),
            "Path(%r).write_text(str(child.pid))" % str(child_pid),
            "while not Path(%r).exists(): time.sleep(.01)" % str(exit_parent),
            "os._exit(23)",
        ])
        profile = ImageProfile(os_version="ubuntu-22.04", cuda="12.8", python="3.10",
                               sglang="test", wheelhouse="/unused", virtualenv="/unused")
        service = SGLangService(profile, command_factory=lambda *_args, **_kwargs: [
            sys.executable, "-c", parent_code])
        result = service.start("prefill", "test", prepare_environment=False)
        if service._process is not None:
            self.groups.append(service._process)
        self.assertTrue(result.accepted, result.detail)
        self.wait_for(child_pid.exists, "child did not start")
        return service, int(child_pid.read_text()), exit_parent

    def test_exited_leader_keeps_children_owned_and_forced_stop_cleans_them(self):
        unrelated = subprocess.Popen([sys.executable, "-c", "import time;time.sleep(120)"],
                                     start_new_session=True)
        self.groups.append(unrelated)
        service, child_pid, exit_parent = self.create_service(ignore_term=True)
        leader = service._process
        exit_parent.touch()
        self.assertEqual(leader.wait(timeout=5), 23)
        self.assertEqual(service.status()["phase"], "FAILED")
        self.assertTrue(self.alive(child_pid))
        # The dead leader must not allow a replacement over its GPU children.
        self.assertFalse(service.start("decode", "next", prepare_environment=False).accepted)
        result = service.stop(timeout=0.1)
        self.assertEqual(result.phase, "STOPPED")
        self.assertTrue(result.timed_out)
        self.assertEqual(result.exit_code, exitcodes.EXIT_STOP_TIMEOUT)
        self.assertFalse(self.alive(child_pid))
        self.assertIsNone(unrelated.poll(), "another session was signalled")
        self.assertEqual(service.status()["service_pid"], 0)
        self.assertEqual(service.stop(timeout=0).phase, "STOPPED")

    def test_graceful_stop_waits_for_leader_and_children(self):
        service, child_pid, _ = self.create_service()
        leader = service._process
        result = service.stop(timeout=2)
        self.assertEqual(result.phase, "STOPPED")
        self.assertFalse(result.timed_out)
        self.assertEqual(result.exit_code, exitcodes.EXIT_OK)
        self.assertIsNotNone(leader.poll())
        self.assertFalse(self.alive(child_pid))


if __name__ == "__main__":
    unittest.main()
