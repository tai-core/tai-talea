"""Wheel filename and lockfile parsing shared by build.py and verify.py.

Deliberately stdlib only, and deliberately tiny: it exists so the producer and
the checker agree, byte for byte, on what a wheel is and on what a pin matches.

The consumer has its own, stricter copy of the first rule in
``provision/ssh_install.py``: a file is only transferred when
``name == os.path.basename(name)`` and it ends in ``.whl``. Everything else in
the bundle is rejected before any byte leaves the control node, so
``is_transferable`` mirrors that gate exactly rather than paraphrasing it.
"""

from __future__ import annotations

import os
import re

WHEEL_SUFFIX = ".whl"

# PEP 427 file name: {distribution}-{version}(-{build})?-{python}-{abi}-{platform}.whl
# Dashes never appear inside the distribution or the version component, which is
# why a non-greedy split on "-" is unambiguous.
_WHEEL = re.compile(
    r"^(?P<name>[^-]+)"
    r"-(?P<version>[^-]+?)"
    r"(?:-(?P<build>\d[^-]*))?"
    r"-(?P<python>[^-]+)"
    r"-(?P<abi>[^-]+)"
    r"-(?P<platform>[^-]+)"
    r"\.whl$",
    re.IGNORECASE,
)

# One requirement line. Extras are accepted and ignored: they do not change
# which distribution the lock pins. Environment markers are parsed off.
_PIN = re.compile(
    r"^(?P<name>[A-Za-z0-9][A-Za-z0-9._-]*)"
    r"(?:\[(?P<extras>[^\]]*)\])?"
    r"(?:\s*(?P<op>===|==|~=|!=|<=|>=|<|>)\s*(?P<version>[^;\s]+))?"
    r"(?:\s*;\s*(?P<marker>.+))?$"
)

# Operators this module is willing to answer for. A lockfile from
# ``pip freeze`` only ever contains ``==``, so nothing else is needed and
# nothing else is guessed at.
VERIFIABLE_OPS = ("==", "===")

SDIST_SUFFIXES = (".tar.gz", ".tgz", ".zip", ".tar.bz2")

# A git host refuses a push carrying any single file larger than this. It is a
# property of the platform, not a preference, and it is the reason the whole
# bundle lives in an artifact store rather than in the repository: `torch` and
# `sglang-kernel` alone are 529 MiB and 354 MiB. Reported, not enforced --
# everything in the local slot is git-ignored anyway.
GIT_FILE_LIMIT = 100 << 20


def canonical(name):
    """PEP 503 canonical distribution name."""
    return re.sub(r"[-_.]+", "-", (name or "").strip()).lower().strip("-")


def is_transferable(name):
    """Whether ``provision/ssh_install.py`` would accept this file name."""
    return name == os.path.basename(name) and name.endswith(WHEEL_SUFFIX)


def is_sdist(name):
    return name.endswith(SDIST_SUFFIXES)


def check_local_slot(directory):
    """Audit the local wheel slot.

    Returns ``(problems, sized)``: reasons this directory would not do what its
    owner meant, and every regular file as ``(name, size)``.

    The hard rule is the file name. The consumer accepts nothing but a bare
    ``*.whl``, and ``build.py`` fills the slot by globbing ``*.whl`` -- so a
    source archive dropped in here is silently *not* an override, which is
    exactly the kind of quiet no-op worth turning into a loud failure.

    Sizes are reported, not enforced. The slot is git-ignored (see
    ``.gitignore``: the internal fork is private and this repository is not),
    so a large artifact here is legitimate when it comes from an artifact
    store. :data:`GIT_FILE_LIMIT` is only interesting as the reason the bundle
    cannot be committed in the first place.
    """
    problems, sized = [], []
    if not directory.is_dir():
        return problems, sized
    for path in sorted(directory.iterdir()):
        if path.is_dir() or path.name in (".gitkeep", "README.md"):
            continue
        sized.append((path.name, path.stat().st_size))
        if not parse_wheel(path.name):
            problems.append(
                "%s 不是合法的轮子文件名，会被无声忽略；这里只放裸的 *.whl" % path.name
            )
    return problems, sized


class Wheel:
    """The identity a wheel file name carries."""

    __slots__ = ("filename", "name", "version", "build", "python", "abi", "platform")

    def __init__(self, filename, name, version, build, python, abi, platform):
        self.filename = filename
        self.name = name
        self.version = version
        self.build = build
        self.python = python
        self.abi = abi
        self.platform = platform

    @property
    def key(self):
        return (canonical(self.name), self.version)

    def __repr__(self):  # pragma: no cover - debugging aid
        return "Wheel(%r)" % self.filename


def parse_wheel(filename):
    """Parse a bare wheel file name, or return ``None`` when it is not one."""
    match = _WHEEL.match(filename or "")
    if not match:
        return None
    return Wheel(
        filename=filename,
        name=match.group("name"),
        version=match.group("version"),
        build=match.group("build"),
        python=match.group("python"),
        abi=match.group("abi"),
        platform=match.group("platform"),
    )


_SDIST = re.compile(r"^(?P<name>.+?)-(?P<version>[^-]+)$")


def parse_sdist(filename):
    """Parse ``<name>-<version>.tar.gz`` (or .tgz/.zip), or return ``None``.

    A source archive is not installable by the consumer -- it never leaves the
    build machine -- but it is what a source-only pin resolves to first, so the
    producer has to recognise one to tell "not fetched" from "needs building".
    """
    for suffix in SDIST_SUFFIXES:
        if filename.endswith(suffix):
            match = _SDIST.match(filename[: -len(suffix)])
            if not match:
                return None
            return (canonical(match.group("name")), match.group("version"))
    return None


def parse_pin(line):
    """Parse one requirement line into ``(canonical_name, op, version)``.

    Returns ``None`` for blanks, comments, option lines and direct references,
    none of which a frozen lockfile should contain and none of which this
    module is prepared to interpret.
    """
    text = (line or "").strip()
    if not text or text.startswith("#") or text.startswith("-"):
        return None
    if "://" in text or "@" in text.split("#", 1)[0]:
        return None
    match = _PIN.match(text)
    if not match:
        return None
    return (canonical(match.group("name")), match.group("op"), match.group("version"))


def _split_local(version):
    base, _, local = (version or "").partition("+")
    return base, local


def pin_matches(op, want, version):
    """Whether ``version`` satisfies a pin written as ``op want``.

    Returns ``True``, ``False``, or ``None`` when the operator is outside
    :data:`VERIFIABLE_OPS` and the honest answer is "not checked" rather than a
    guess. A pin without a local version segment matches an artifact that has
    one: PEP 440 says ``==2.13.0`` accepts ``2.13.0+cu130``, which is exactly
    why stripping local segments is safe at install time and unsafe at
    download time.
    """
    if not op or not want:
        return True
    if op == "===":
        return version == want
    if op == "==":
        want_base, want_local = _split_local(want)
        got_base, _ = _split_local(version)
        if want_local:
            return version == want
        return got_base == want_base
    return None


def pin_matches_strict(op, want, version):
    """Like :func:`pin_matches` but without the local-segment allowance."""
    if not op or not want:
        return version is not None
    if op == "===":
        return version == want
    if op == "==":
        return version == want
    return None


def strip_local(pin):
    """Drop the local version segment from a pin, or return ``None``."""
    parsed = parse_pin(pin)
    if not parsed or not parsed[1] or not parsed[2]:
        return None
    name, op, version = parsed
    base, local = _split_local(version)
    if not local:
        return None
    return "%s%s%s" % (name, op, base)
