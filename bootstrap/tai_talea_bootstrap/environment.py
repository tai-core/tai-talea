"""Offline Python environment preparation for the controlled image route.

§10 (M1): business code and light dependencies are installed from an offline
wheelhouse into a dedicated virtualenv. The running service is never upgraded in
place, and a failed installation releases the instance instead of degrading a
live worker.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sysconfig


class EnvironmentError_(RuntimeError):
    """Raised when the virtualenv or the offline install cannot be prepared."""

    def __init__(self, message, *, exit_code=None, output=""):
        super().__init__(message)
        self.exit_code = exit_code
        self.output = output


class EnvironmentInstaller:
    """Creates and populates the virtualenv of one container.

    Every command is executed as an argument vector: the bootstrap never runs a
    shell, so a wheelhouse path or a package name can never turn into arbitrary
    command execution (§9 and §15).
    """

    def __init__(self, profile, runner=None, python=None):
        self.profile = profile
        self._runner = runner or _run
        self._python = python or sysconfig.get_path("scripts") or "python3"

    @property
    def python_executable(self):
        """Path of the interpreter inside the managed virtualenv."""
        return os.path.join(self.profile.virtualenv, "bin", "python")

    def requirements_path(self):
        return os.path.join(self.profile.wheelhouse, "requirements.txt")

    def ensure_virtualenv(self):
        """Create the virtualenv if it does not exist yet."""
        if os.path.isdir(self.profile.virtualenv) and os.path.isfile(self.python_executable):
            return self.python_executable
        parent = os.path.dirname(self.profile.virtualenv.rstrip("/")) or "/"
        if parent and not os.path.isdir(parent):
            os.makedirs(parent, exist_ok=True)
        result = self._runner(["python3", "-m", "venv", self.profile.virtualenv])
        if result.returncode != 0:
            raise EnvironmentError_(
                "creating the virtualenv at %s failed" % self.profile.virtualenv,
                output=result.stderr or result.stdout,
            )
        return self.python_executable

    def install_requirements(self, offline=True):
        """Install the pinned requirements from the offline wheelhouse.

        ``offline`` keeps pip from reaching the network, which is the whole point
        of the controlled image route.
        """
        if not os.path.isdir(self.profile.wheelhouse):
            raise EnvironmentError_("wheelhouse %s does not exist" % self.profile.wheelhouse)
        requirements = self.requirements_path()
        if not os.path.isfile(requirements):
            raise EnvironmentError_("wheelhouse %s has no requirements.txt" % self.profile.wheelhouse)

        command = [
            self.python_executable,
            "-m",
            "pip",
            "install",
            "--no-index" if offline else "--index-url",
        ]
        if offline:
            command += ["--find-links", self.profile.wheelhouse]
        else:
            command += ["https://pypi.org/simple"]
        command += ["-r", requirements]

        result = self._runner(command)
        if result.returncode != 0:
            raise EnvironmentError_(
                "installing requirements from %s failed" % requirements,
                output=result.stderr or result.stdout,
            )
        return requirements

    def prepare(self, offline=True):
        """Prepare the environment and return the interpreter to use."""
        self.ensure_virtualenv()
        self.install_requirements(offline=offline)
        return self.python_executable


class _Completed:
    def __init__(self, returncode, stdout="", stderr=""):
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr


def _run(command, timeout=1800):
    """Execute one command without a shell."""
    try:
        completed = subprocess.run(
            command, capture_output=True, text=True, timeout=timeout, check=False
        )
    except OSError as error:
        return _Completed(127, "", str(error))
    except subprocess.SubprocessError as error:
        return _Completed(124, "", str(error))
    return _Completed(completed.returncode, completed.stdout or "", completed.stderr or "")


def ensure_work_directories(paths):
    """Create the working and log directories owned by the bootstrap (§7.1 step 4)."""
    created = []
    for path in paths:
        if not path:
            continue
        os.makedirs(path, exist_ok=True)
        created.append(path)
    return created


def disk_free_bytes(path):
    """Report free space so preparation can fail early on a full volume."""
    try:
        return shutil.disk_usage(path).free
    except OSError:
        return 0


__all__ = [
    "EnvironmentError_",
    "EnvironmentInstaller",
    "ensure_work_directories",
    "disk_free_bytes",
]
