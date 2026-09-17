"""Controlled image compatibility matrix (§10, M1 controlled image route).

Milestone 1 uses the partner supplied baseline image: the OS, CUDA, Python and
SGLang major versions are fixed. Everything else must be verified before any
service starts, and a mismatch releases the instance instead of producing a
half-working worker.
"""

from __future__ import annotations

import os
import platform
import re
import shutil
import subprocess
from dataclasses import dataclass, field


class ProfileError(RuntimeError):
    """Raised when the container does not match the controlled image profile."""

    def __init__(self, mismatches):
        self.mismatches = list(mismatches)
        super().__init__("; ".join(self.mismatches) or "environment mismatch")


def _read_first_line(path):
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return handle.readline().strip()
    except OSError:
        return ""


def _detect_os():
    """Return "<distro-id>-<version-id>" such as ``ubuntu-22.04``.

    ``/etc/os-release`` is the portable source of truth on the controlled
    baseline images; the platform module is only a fallback.
    """
    identifier = ""
    version = ""
    try:
        with open("/etc/os-release", "r", encoding="utf-8") as handle:
            for line in handle:
                key, _, value = line.strip().partition("=")
                value = value.strip().strip('"')
                if key == "ID":
                    identifier = value
                elif key == "VERSION_ID":
                    version = value
    except OSError:
        identifier = ""
    if identifier:
        return "%s-%s" % (identifier, version) if version else identifier
    return (platform.system() or "").lower()


def _detect_cuda():
    version = _read_first_line("/usr/local/cuda/version.txt")
    if version:
        match = re.search(r"CUDA Version (\d+\.\d+)", version)
        if match:
            return match.group(1)
    nvcc = shutil.which("nvcc")
    if nvcc:
        try:
            completed = subprocess.run(
                [nvcc, "--version"], capture_output=True, text=True, timeout=10, check=False
            )
            match = re.search(r"release (\d+\.\d+)", completed.stdout)
            if match:
                return match.group(1)
        except (OSError, subprocess.SubprocessError):
            return ""
    return ""


def _detect_python():
    return "%d.%d" % (os.sys.version_info.major, os.sys.version_info.minor)


def virtualenv_python(profile):
    """Interpreter inside the managed virtualenv, or "" when it is absent."""
    if not profile or not (profile.virtualenv or "").strip():
        return ""
    for candidate in (
        os.path.join(profile.virtualenv, "bin", "python"),
        os.path.join(profile.virtualenv, "Scripts", "python.exe"),
    ):
        if os.path.isfile(candidate):
            return candidate
    return ""


def _detect_sglang(profile):
    """Read the installed SGLang version from the managed virtualenv.

    The interpreter comes from the profile's virtualenv, never from the running
    bootstrap process: the version that matters is the one the service will
    actually be launched with.
    """
    python = virtualenv_python(profile)
    if not python:
        return ""
    probe = "import sglang, sys; sys.stdout.write(getattr(sglang, '__version__', ''))"
    try:
        completed = subprocess.run(
            [python, "-c", probe], capture_output=True, text=True, timeout=60, check=False
        )
    except (OSError, subprocess.SubprocessError):
        return ""
    return completed.stdout.strip()


def _major(version):
    """Return the major component of a version string, ignoring wildcards."""
    if version is None:
        return ""
    cleaned = version.strip()
    if not cleaned or cleaned.endswith(".x"):
        return cleaned[:-2] if cleaned.endswith(".x") else cleaned
    return cleaned.split(".")[0]


def _matches(want, got):
    """Compare a required version against an observed one.

    A requirement such as ``0.4.x`` accepts any ``0.4.*`` version. An exact
    requirement must match exactly.
    """
    want = (want or "").strip()
    got = (got or "").strip()
    if not want:
        return True
    if want.endswith(".x"):
        return got.startswith(want[:-1])
    return got == want


@dataclass
class ImageProfile:
    """The baseline image contract the container must satisfy."""

    os_version: str
    cuda: str
    python: str
    sglang: str
    wheelhouse: str
    virtualenv: str
    bootstrap_version: str = ""
    model_id: str = ""

    def validate(self):
        required = {
            "os": self.os_version,
            "cuda": self.cuda,
            "python": self.python,
            "sglang": self.sglang,
            "wheelhouse": self.wheelhouse,
            "virtualenv": self.virtualenv,
        }
        missing = [name for name, value in required.items() if not (value or "").strip()]
        if missing:
            raise ProfileError(["profile field %s is required" % name for name in missing])

    def as_dict(self):
        return {
            "os": self.os_version,
            "cuda": self.cuda,
            "python": self.python,
            "sglang": self.sglang,
            "wheelhouse": self.wheelhouse,
            "virtualenv": self.virtualenv,
            "bootstrap_version": self.bootstrap_version,
        }


@dataclass
class EnvironmentReport:
    """What the container actually is."""

    os: str = ""
    cuda: str = ""
    python: str = ""
    sglang: str = ""
    wheelhouse: str = ""
    virtualenv: str = ""
    mismatches: list = field(default_factory=list)

    @property
    def compatible(self):
        return not self.mismatches

    def as_dict(self):
        return {
            "os": self.os,
            "cuda": self.cuda,
            "python": self.python,
            "sglang": self.sglang,
            "wheelhouse": self.wheelhouse,
            "virtualenv": self.virtualenv,
        }


def detect_environment(profile, sglang_version_probe=None):
    """Inspect the container and compare it with the required profile.

    ``sglang_version_probe`` exists so tests can supply the version without a
    real virtualenv.
    """
    profile.validate()
    report = EnvironmentReport(
        os=_detect_os(),
        cuda=_detect_cuda(),
        python=_detect_python(),
        sglang=(sglang_version_probe or _detect_sglang)(profile),
        wheelhouse=profile.wheelhouse if os.path.isdir(profile.wheelhouse) else "",
        virtualenv=profile.virtualenv if os.path.isdir(profile.virtualenv) else "",
    )

    if not _matches(profile.os_version, report.os):
        report.mismatches.append("os=%s does not match profile %s" % (report.os, profile.os_version))
    if not _matches(profile.cuda, report.cuda):
        report.mismatches.append("cuda=%s does not match profile %s" % (report.cuda, profile.cuda))
    if not _matches(profile.python, report.python):
        report.mismatches.append("python=%s does not match profile %s" % (report.python, profile.python))
    if not _matches(profile.sglang, report.sglang):
        report.mismatches.append("sglang=%s does not match profile %s" % (report.sglang, profile.sglang))
    if profile.wheelhouse and not report.wheelhouse:
        report.mismatches.append("wheelhouse %s does not exist" % profile.wheelhouse)
    if profile.virtualenv and not report.virtualenv:
        report.mismatches.append("virtualenv %s does not exist" % profile.virtualenv)
    return report


def require_compatible(profile, sglang_version_probe=None):
    """Return the environment report or raise ``ProfileError``."""
    report = detect_environment(profile, sglang_version_probe=sglang_version_probe)
    if not report.compatible:
        raise ProfileError(report.mismatches)
    return report
