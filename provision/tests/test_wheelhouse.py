"""Tests for the offline wheelhouse pipeline.

The producer and the checker are both command line entry points that the fleet
depends on, so they are exercised as subprocesses end to end with a stand-in
pip. The stand-in is what makes this possible offline: it answers ``freeze``
from the environment and writes one file per requested pin, so the whole
``build.py -> verify.py`` path runs without a network or a real index.
"""

import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).parents[1]
WHEELHOUSE = ROOT / "wheelhouse"
REPO = ROOT.parent
REAL_SPEC = WHEELHOUSE / "spec.json"
IMAGE_PROFILE = REPO / "deploy" / "container" / "profile.json"

FAKE_PIP = '''\
import os
import sys

# Pins that exist only as a source archive, like cuda-tile 1.6.0rc5 on PyPI.
SDIST_ONLY = {"cuda-tile"}
# Pins the fake index refuses, to exercise the unresolved path.
BLOCKED = {item for item in os.environ.get("FAKE_PIP_BLOCKED", "").split(",") if item}
PAIRS = (
    "-d", "--dest", "--index-url", "--extra-index-url", "--platform", "--abi",
    "--python-version", "--wheel-dir",
)


def wheel_name(pin):
    name, _, version = pin.partition("==")
    if not version:
        return None
    if name in SDIST_ONLY:
        return "%s-%s.tar.gz" % (name.replace("-", "_"), version)
    return "%s-%s-py3-none-any.whl" % (name.replace("-", "_"), version)


def main(argv):
    if not argv or argv[0] == "--version":
        print("pip 99.0 (fake)")
        return 0
    if argv[0] == "freeze":
        for line in os.environ.get("FAKE_PIP_FREEZE", "").split(","):
            if line.strip():
                print(line.strip())
        return 0
    if argv[0] == "download":
        dest, pins, index = ".", [], 1
        while index < len(argv):
            item = argv[index]
            if item in PAIRS:
                if item in ("-d", "--dest"):
                    dest = argv[index + 1]
                index += 2
                continue
            if item.startswith("-"):
                index += 1
                continue
            pins.append(item)
            index += 1
        failed = []
        for pin in pins:
            name = pin.partition("==")[0]
            if name in BLOCKED:
                failed.append(pin)
                continue
            target = wheel_name(pin)
            if target is None:
                continue
            with open(os.path.join(dest, target), "wb") as handle:
                handle.write(b"PK\\x03\\x04 fake " + pin.encode())
        if failed:
            for pin in failed:
                sys.stderr.write("ERROR: No matching distribution found for %s\\n" % pin)
            return 1
        return 0
    if argv[0] == "wheel":
        dest, sources, index = ".", [], 1
        while index < len(argv):
            item = argv[index]
            if item in PAIRS:
                if item == "--wheel-dir":
                    dest = argv[index + 1]
                index += 2
                continue
            if item.startswith("-"):
                index += 1
                continue
            sources.append(item)
            index += 1
        for source in sources:
            stem = os.path.basename(source)
            for suffix in (".tar.gz", ".zip", ".tgz"):
                if stem.endswith(suffix):
                    stem = stem[: -len(suffix)]
                    break
            with open(os.path.join(dest, stem + "-py3-none-any.whl"), "wb") as handle:
                handle.write(b"PK\\x03\\x04 built from " + os.path.basename(source).encode())
        return 0
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
'''

BASE_LOCK = [
    "sglang==0.5.20",
    "mooncake-transfer-engine==0.3.13.post1",
    "runai-model-streamer==0.13.0",
    "nvidia-cuda-runtime-cu12==12.9.79",
    "ninja==1.13.0",
    "torch==2.13.0+cu130",
]


def load(name):
    spec = importlib.util.spec_from_file_location(name, WHEELHOUSE / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class WheelSpecTests(unittest.TestCase):
    def setUp(self):
        self.wheelspec = load("wheelspec")

    def test_wheel_name_is_parsed_with_the_local_version(self):
        wheel = self.wheelspec.parse_wheel(
            "torch-2.13.0+cu130-cp312-cp312-manylinux_2_28_x86_64.whl"
        )
        self.assertEqual(("torch", "2.13.0+cu130"), wheel.key)
        self.assertEqual("cp312", wheel.python)

    def test_multi_platform_wheel_name_is_parsed(self):
        wheel = self.wheelspec.parse_wheel(
            "ninja-1.13.0-py3-none-manylinux_2_17_x86_64.manylinux2014_x86_64.whl"
        )
        self.assertEqual(("ninja", "1.13.0"), wheel.key)
        self.assertEqual("py3", wheel.python)

    def test_non_wheel_names_are_rejected(self):
        for name in ("cuda_tile-1.6.0rc5.tar.gz", "manifest.json", "requirements.txt"):
            self.assertIsNone(self.wheelspec.parse_wheel(name), name)

    def test_a_stable_abi_wheel_name_is_parsed(self):
        wheel = self.wheelspec.parse_wheel(
            "sgl_kernel-0.3.21-cp310-abi3-manylinux2014_x86_64.whl"
        )
        self.assertEqual(("sgl-kernel", "0.3.21"), wheel.key)
        self.assertEqual("cp310", wheel.python)
        self.assertEqual("abi3", wheel.abi)

    def test_distribution_names_are_canonicalised(self):
        self.assertEqual("mooncake-transfer-engine", self.wheelspec.canonical("mooncake_transfer_engine"))
        self.assertEqual("flashinfer-python", self.wheelspec.canonical("flashinfer.python"))

    def test_a_pin_without_a_local_version_accepts_an_artifact_with_one(self):
        self.assertTrue(self.wheelspec.pin_matches("==", "2.13.0", "2.13.0+cu130"))
        self.assertFalse(self.wheelspec.pin_matches("==", "2.13.0+cu130", "2.13.0"))
        self.assertFalse(self.wheelspec.pin_matches("==", "0.5.19", "0.5.20"))

    def test_unverifiable_operators_report_not_checked(self):
        self.assertIsNone(self.wheelspec.pin_matches(">=", "1.0", "2.0"))

    def test_the_installer_file_name_gate_is_mirrored(self):
        self.assertTrue(self.wheelspec.is_transferable("sglang-0.5.20-py3-none-any.whl"))
        self.assertFalse(self.wheelspec.is_transferable("wheels/sglang-0.5.20-py3-none-any.whl"))
        self.assertFalse(self.wheelspec.is_transferable("sglang-0.5.20.tar.gz"))

    def test_local_version_segments_can_be_stripped(self):
        self.assertEqual("torch==2.13.0", self.wheelspec.strip_local("torch==2.13.0+cu130"))
        self.assertIsNone(self.wheelspec.strip_local("ninja==1.13.0"))

    def test_the_local_slot_accepts_a_fork_wheel(self):
        slot = self.slot()
        (slot / "sglang-0.5.20-py3-none-any.whl").write_bytes(b"PK\x03\x04")
        (slot / ".gitkeep").write_bytes(b"")
        (slot / "README.md").write_text("docs", encoding="utf-8")
        problems, sized = self.wheelspec.check_local_slot(slot)
        self.assertEqual([], problems)
        self.assertEqual(
            [("sglang-0.5.20-py3-none-any.whl", 4)], sized
        )

    def test_the_local_slot_rejects_anything_but_a_bare_wheel(self):
        slot = self.slot()
        (slot / "cuda_tile-1.6.0rc5.tar.gz").write_bytes(b"src")
        problems, _ = self.wheelspec.check_local_slot(slot)
        self.assertEqual(1, len(problems))
        self.assertIn("cuda_tile-1.6.0rc5.tar.gz", problems[0])
        # The point of the rule: build.py fills the slot by globbing *.whl, so
        # an archive here is silently not an override.
        self.assertIn("无声忽略", problems[0])

    def test_a_large_local_wheel_is_reported_rather_than_refused(self):
        # The slot is git-ignored, so a 529 MiB torch wheel in it is legal --
        # it simply can never be committed. Size is information, not a gate.
        slot = self.slot()
        (slot / "torch-2.14.1+cu130-cp312-cp312-manylinux_2_34_x86_64.whl").write_bytes(
            b"PK\x03\x04!"
        )
        problems, sized = self.wheelspec.check_local_slot(slot)
        self.assertEqual([], problems)
        self.assertEqual(5, sized[0][1])
        self.assertLess(sized[0][1], self.wheelspec.GIT_FILE_LIMIT)

    def test_the_git_file_limit_is_the_one_the_platform_enforces(self):
        self.assertEqual(100, self.wheelspec.GIT_FILE_LIMIT >> 20)

    def test_an_absent_slot_is_not_a_problem(self):
        empty = Path(tempfile.mkdtemp(prefix="talea-absent-"))
        self.addCleanup(shutil.rmtree, empty, True)
        self.assertEqual(([], []), self.wheelspec.check_local_slot(empty / "nope"))

    def slot(self):
        directory = Path(tempfile.mkdtemp(prefix="talea-slot-"))
        self.addCleanup(shutil.rmtree, directory, True)
        return directory


class WheelTagTests(unittest.TestCase):
    """The interpreter gate, exercised directly rather than by accident.

    ``sgl-kernel`` ships a stable-ABI wheel (``cp310-abi3``) and it is the
    single largest artifact in the bundle, so the direction of the abi3 rule
    deserves its own test: forwards yes, backwards no.
    """

    def setUp(self):
        self.wheelspec = load("wheelspec")
        sys.modules["wheelspec"] = self.wheelspec
        self.verify = load("verify")

    def accepts(self, filename, python):
        wheel = self.wheelspec.parse_wheel(filename)
        return self.verify.python_tag_ok(wheel, python)[0]

    def test_a_stable_abi_wheel_installs_on_a_newer_interpreter(self):
        self.assertTrue(
            self.accepts("sgl_kernel-0.3.21-cp310-abi3-manylinux2014_x86_64.whl", "3.12")
        )

    def test_a_stable_abi_wheel_does_not_go_backwards(self):
        self.assertFalse(
            self.accepts("sgl_kernel-0.3.21-cp313-abi3-manylinux2014_x86_64.whl", "3.12")
        )

    def test_an_ordinary_interpreter_tag_still_has_to_match(self):
        self.assertFalse(
            self.accepts("flashinfer_python-0.6.18-cp310-cp310-manylinux2014_x86_64.whl", "3.12")
        )
        self.assertTrue(
            self.accepts("flashinfer_python-0.6.18-cp312-cp312-manylinux2014_x86_64.whl", "3.12")
        )

    def test_an_interpreter_independent_wheel_is_always_accepted(self):
        self.assertTrue(self.accepts("sglang-0.5.20-py3-none-any.whl", "3.12"))


class WheelhousePipelineTests(unittest.TestCase):
    """build.py and verify.py, driven through their command line."""

    def setUp(self):
        self.directory = Path(tempfile.mkdtemp(prefix="talea-wheelhouse-"))
        self.addCleanup(shutil.rmtree, self.directory, True)
        self.spec = self.directory / "spec.json"
        self.out = self.directory / "out"
        self.fake_pip = self.directory / "fake_pip.py"
        self.profile = self.directory / "profile.json"
        self.fake_pip.write_text(FAKE_PIP, encoding="utf-8")
        shutil.copy2(REAL_SPEC, self.spec)
        shutil.copy2(IMAGE_PROFILE, self.profile)
        self.lock = self.directory / "lock.txt"
        self.write_lock(BASE_LOCK)
        self.env = dict(os.environ, PYTHONIOENCODING="utf-8", PYTHONDONTWRITEBYTECODE="1")

    # -- helpers ----------------------------------------------------------

    def write_lock(self, lines):
        self.lock.write_text("\n".join(lines) + "\n", encoding="utf-8")

    def pip_argument(self):
        # Forward slashes: shlex would eat the backslashes in a Windows path.
        return "%s %s" % (self.python(), self.fake_pip.as_posix())

    @staticmethod
    def python():
        return Path(sys.executable).as_posix()

    def run_build(self, *extra, spec=None, lock=None, expect=0):
        argv = [
            self.python(),
            str(WHEELHOUSE / "build.py"),
            "--spec",
            str(spec or self.spec),
            "--output",
            str(self.out),
            "--requirements",
            str(lock or self.lock),
            "--pip",
            self.pip_argument(),
        ] + list(extra)
        result = subprocess.run(
            argv, capture_output=True, text=True, encoding="utf-8", env=self.env
        )
        self.assertEqual(expect, result.returncode, result.stderr)
        return result

    def run_verify(self, *extra, wheelhouse=None, spec=None, profile=None):
        argv = [
            self.python(),
            str(WHEELHOUSE / "verify.py"),
            "--wheelhouse",
            str(wheelhouse or self.out),
            "--spec",
            str(spec or self.spec),
            "--profile",
            str(profile or self.profile),
        ] + list(extra)
        result = subprocess.run(
            argv, capture_output=True, text=True, encoding="utf-8", env=self.env
        )
        return result, json.loads(result.stdout) if "--json" in extra else None

    def patch_spec(self, mutate):
        spec = json.loads(self.spec.read_text(encoding="utf-8"))
        mutate(spec)
        patched = self.directory / "spec-patched.json"
        patched.write_text(json.dumps(spec, ensure_ascii=False), encoding="utf-8")
        return patched

    def manifest(self):
        return json.loads((self.out / "manifest.json").read_text(encoding="utf-8"))

    # -- tests ------------------------------------------------------------

    def test_a_fresh_bundle_builds_and_verifies(self):
        self.run_build()
        manifest = self.manifest()
        names = sorted(item["name"] for item in manifest["files"])
        self.assertEqual(len(BASE_LOCK), len(names))
        self.assertIn("sglang-0.5.20-py3-none-any.whl", names)
        self.assertEqual(BASE_LOCK, (self.out / "requirements.txt").read_text(encoding="utf-8").split())
        self.assertEqual(6, manifest["requirements_count"])
        self.assertEqual("ubuntu-24.04", manifest["profile"]["os"])

        result, payload = self.run_verify("--json")
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertTrue(payload["ok"], payload["problems"])

    def test_the_reference_environment_supplies_the_lock(self):
        self.env["FAKE_PIP_FREEZE"] = ",".join(BASE_LOCK)
        argv = [
            self.python(),
            str(WHEELHOUSE / "build.py").replace("\\", "/"),
            "--spec",
            str(self.spec),
            "--output",
            str(self.out),
            "--venv",
            str(self.directory / "reference-venv"),
            "--pip",
            self.pip_argument(),
        ]
        result = subprocess.run(
            argv, capture_output=True, text=True, encoding="utf-8", env=self.env
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(6, self.manifest()["requirements_count"])

    def test_a_reference_environment_missing_a_contract_package_is_refused(self):
        self.write_lock([line for line in BASE_LOCK if not line.startswith("ninja")])
        result = self.run_build(expect=1)
        self.assertIn("ninja", result.stderr)

    def test_a_pin_that_resolves_to_nothing_fails_the_build(self):
        self.env["FAKE_PIP_BLOCKED"] = "mooncake-transfer-engine"
        result = self.run_build(expect=1)
        self.assertIn("mooncake-transfer-engine", result.stderr)
        self.assertIn("既不在本地轮子里", result.stderr)

    def test_a_source_only_pin_is_built_into_a_wheel(self):
        self.write_lock(BASE_LOCK + ["cuda-tile==1.6.0rc5"])
        self.run_build()
        names = sorted(item["name"] for item in self.manifest()["files"])
        self.assertIn("cuda_tile-1.6.0rc5-py3-none-any.whl", names)
        self.assertFalse(any(name.endswith(".tar.gz") for name in names))
        self.assertEqual(0, self.run_verify("--json")[0].returncode)

    def test_a_source_only_pin_without_a_builder_is_refused(self):
        self.write_lock(BASE_LOCK + ["cuda-tile==1.6.0rc5"])
        spec = self.patch_spec(lambda data: data["builder"].update(build_sdists=False))
        result = self.run_build(expect=1, spec=spec)
        self.assertIn("cuda-tile", result.stderr)
        self.assertIn(".whl", result.stderr)

    def test_a_tampered_artifact_is_rejected(self):
        self.run_build()
        target = self.out / "sglang-0.5.20-py3-none-any.whl"
        target.write_bytes(target.read_bytes() + b"x")
        result, payload = self.run_verify("--json")
        self.assertEqual(1, result.returncode)
        self.assertTrue(any("sha256" in problem for problem in payload["problems"]))

    def test_a_pin_without_an_artifact_is_rejected(self):
        self.run_build()
        with (self.out / "requirements.txt").open("a", encoding="utf-8") as handle:
            handle.write("flashinfer-python==0.6.18\n")
        result, payload = self.run_verify("--json")
        self.assertEqual(1, result.returncode)
        self.assertTrue(
            any("flashinfer-python" in problem for problem in payload["problems"]), payload
        )

    def test_an_unregistered_file_in_the_bundle_is_rejected(self):
        self.run_build()
        (self.out / "cuda_tile-1.6.0rc5.tar.gz").write_bytes(b"stray")
        result, payload = self.run_verify("--json")
        self.assertEqual(1, result.returncode)
        self.assertTrue(any("tar.gz" in problem for problem in payload["problems"]))

    def test_a_wheel_for_the_wrong_interpreter_is_rejected(self):
        stub = self.directory / "stub"
        stub.mkdir()
        (stub / "flashinfer_python-0.6.18-cp310-cp310-manylinux2014_x86_64.whl").write_bytes(
            b"PK\x03\x04 cp310"
        )
        self.write_lock(BASE_LOCK + ["flashinfer-python==0.6.18"])
        spec = self.patch_spec(
            lambda data: data["local_wheels"].update(paths=["stub/*.whl"])
        )
        self.run_build(spec=spec)
        self.assertIn(
            "flashinfer_python-0.6.18-cp310-cp310-manylinux2014_x86_64.whl",
            [item["name"] for item in self.manifest()["files"]],
        )
        result, payload = self.run_verify("--json", spec=spec)
        self.assertEqual(1, result.returncode)
        self.assertTrue(any("python 标签" in problem for problem in payload["problems"]), payload)

    def test_a_local_wheel_overrides_the_index(self):
        stub = self.directory / "stub"
        stub.mkdir()
        (stub / "sglang-0.5.20-py3-none-any.whl").write_bytes(b"PK\x03\x04 local fork")
        spec = self.patch_spec(
            lambda data: data["local_wheels"].update(paths=["stub/*.whl"])
        )
        self.run_build(spec=spec)
        artifact = self.out / "sglang-0.5.20-py3-none-any.whl"
        self.assertEqual(b"PK\x03\x04 local fork", artifact.read_bytes())
        self.assertEqual(1, self.manifest()["artifact_sources"]["counts"]["local"])
        self.assertEqual(0, self.run_verify("--json", spec=spec)[0].returncode)

    def test_the_local_wheels_slot_is_laid_down_before_the_index(self):
        slot = self.directory / "wheels"
        slot.mkdir()
        (slot / ".gitkeep").write_bytes(b"")
        (slot / "README.md").write_text("docs", encoding="utf-8")
        (slot / "sglang-0.5.20-py3-none-any.whl").write_bytes(b"PK\x03\x04 fork")
        self.run_build()
        self.assertEqual(b"PK\x03\x04 fork", (self.out / "sglang-0.5.20-py3-none-any.whl").read_bytes())
        self.assertEqual(1, self.manifest()["artifact_sources"]["counts"]["local"])
        self.assertEqual(0, self.run_verify("--json")[0].returncode)

    def test_a_wheels_slot_holding_a_source_archive_stops_the_build(self):
        slot = self.directory / "wheels"
        slot.mkdir()
        (slot / "cuda_tile-1.6.0rc5.tar.gz").write_bytes(b"src")
        result = self.run_build(expect=1)
        self.assertIn("cuda_tile-1.6.0rc5.tar.gz", result.stdout + result.stderr)

    def test_an_image_profile_that_disagrees_with_the_bundle_is_rejected(self):
        self.run_build()
        profile = json.loads(self.profile.read_text(encoding="utf-8"))
        profile["os"] = "ubuntu-22.04"
        profile["python"] = "3.10"
        drifted = self.directory / "profile-drifted.json"
        drifted.write_text(json.dumps(profile), encoding="utf-8")
        result, payload = self.run_verify("--json", profile=drifted)
        self.assertEqual(1, result.returncode)
        self.assertTrue(any("ubuntu-22.04" in problem for problem in payload["problems"]))

    def test_a_bundle_from_a_different_spec_is_rejected(self):
        self.run_build()
        spec = self.patch_spec(lambda data: data["profile"].update(sglang="0.5.19"))
        result, payload = self.run_verify("--json", spec=spec)
        self.assertEqual(1, result.returncode)
        self.assertTrue(any("sglang" in problem for problem in payload["problems"]))

    def test_the_reported_hashes_match_the_files_on_disk(self):
        result = self.run_build("--json")
        payload = json.loads(result.stdout)
        manifest_bytes = (self.out / "manifest.json").read_bytes()
        self.assertEqual(
            hashlib.sha256(manifest_bytes).hexdigest(), payload["wheel_manifest_sha256"]
        )
        self.assertEqual(
            hashlib.sha256((self.out / "requirements.txt").read_bytes()).hexdigest(),
            payload["requirements_sha256"],
        )

    def test_clean_removes_only_what_the_tool_owns(self):
        self.run_build()
        bystander = self.out / "NOTES.md"
        bystander.write_text("keep me", encoding="utf-8")
        self.run_build("--clean")
        self.assertTrue(bystander.is_file())
        self.assertTrue((self.out / "manifest.json").is_file())

    def test_verifying_a_directory_without_a_manifest_is_a_clean_failure(self):
        self.out.mkdir()
        result, payload = self.run_verify("--json")
        self.assertEqual(1, result.returncode)
        self.assertTrue(any("manifest.json" in problem for problem in payload["problems"]))


if __name__ == "__main__":
    unittest.main()
