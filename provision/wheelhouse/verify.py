#!/usr/bin/env python3
"""Check a wheelhouse against the contract its consumer will enforce.

Every check here exists because the corresponding check exists on the other
side of the wire, and finding out there costs a container:

* ``provision/ssh_install.py`` rejects any bundle file whose name is not a bare
  ``.whl`` and whose SHA256 disagrees with ``manifest.json`` -- so both are
  verified here first, and the two files the installer skips
  (``manifest.json``, ``requirements.txt``) are handled explicitly.
* ``provision/install_node.py`` runs ``pip install --no-index --find-links``
  over the frozen lock -- so every pin is required to have an artifact, and
  ``--install-check`` can run that resolution for real before shipping.
* ``bootstrap/tai_talea_bootstrap/profile.py`` compares the container against
  the image profile and releases the instance on a mismatch -- so the bundle's
  profile, the image profile and the actual wheel tags are compared here, where
  the failure is free.

Exit code is 0 when there are no problems, 1 otherwise. Warnings do not fail
the run unless ``--strict`` is given.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import shlex
import subprocess
import sys
from pathlib import Path

import wheelspec

MANIFEST_NAME = "manifest.json"
LOCK_NAME = "requirements.txt"
SPEC_NAME = "spec.json"
MANIFEST_SCHEMA = "talea-wheelhouse/1"
PROFILE_FIELDS = ("os", "cuda", "python", "sglang", "wheelhouse", "virtualenv")


def sha256_file(path, chunk=4 << 20):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(chunk), b""):
            digest.update(block)
    return digest.hexdigest()


def matches(want, got):
    """Same version rule as ``bootstrap/tai_talea_bootstrap/profile.py``.

    A trailing ``.x`` is a wildcard: ``13.x`` accepts ``13.0`` and ``13.1``.
    Anything else must be equal, because a profile that is approximately right
    is a profile that releases instances.
    """
    want = str(want or "").strip()
    got = str(got or "").strip()
    if not want:
        return True
    if want.endswith(".x"):
        return got.startswith(want[:-1])
    return got == want


def read_json(path):
    try:
        return json.loads(Path(path).read_text(encoding="utf-8"))
    except OSError as error:
        raise ValueError("读不到 %s：%s" % (path, error))
    except ValueError as error:
        raise ValueError("%s 不是合法 JSON：%s" % (path, error))


def python_tag_ok(wheel, profile_python):
    """Whether the profiled interpreter could install this wheel.

    ``py3`` / ``py2.py3`` / ``none`` are interpreter independent. A ``cpXYZ``
    tag normally has to name the profiled minor version: a wheelhouse frozen by
    the wrong interpreter installs nothing on the node, and the failure there
    is buried in a pip resolver dump.

    The exception is the stable ABI. ``cp310-abi3`` is compiled once and
    installs on cp310 *and every later* CPython -- which is how ``sgl-kernel``
    ships, and that is the single largest artifact in the bundle, so treating
    it as a mismatch is not a hypothetical. The direction is load-bearing:
    ``cp313-abi3`` is a 3.13-and-up wheel and does not go into a 3.12 venv.
    """
    major, _, minor = str(profile_python or "").partition(".")
    if not minor.isdigit():
        return True, ""
    wanted = int("%s%s" % (major, minor))
    stable = "abi3" in [part.lower() for part in wheel.abi.split(".")]
    tokens = [token for token in wheel.python.split(".") if token]
    if not tokens:
        return True, ""
    for token in tokens:
        lowered = token.lower()
        if lowered in ("py3", "py2.py3", "py2", "none", "any"):
            continue
        if lowered.startswith("cp"):
            digits = lowered[2:]
            if not digits.isdigit():
                return True, token
            version = int(digits)
            if version == wanted:  # exact match
                continue
            if stable and version <= wanted:
                continue
            return False, token
        return True, token
    return True, ""


def check_manifest(directory, manifest):
    problems, warnings = [], []
    entries = manifest.get("files")
    if not isinstance(entries, list):
        return ["manifest.json 缺少 files 列表"], warnings, {}

    listed, by_key = {}, {}
    for entry in entries:
        if not isinstance(entry, dict):
            problems.append("manifest.json 的 files 里有非对象条目")
            continue
        name = str(entry.get("name", ""))
        if not name:
            problems.append("manifest.json 有条目缺 name")
            continue
        if not wheelspec.is_transferable(name):
            problems.append(
                "%s 过不了 install_node.py 的文件名闸门（必须是裸文件名且以 .whl 结尾）" % name
            )
            listed[name] = entry
            continue
        if name in listed:
            problems.append("manifest.json 重复登记 %s" % name)
            continue
        listed[name] = entry
        path = directory / name
        if not path.is_file():
            problems.append("登记了 %s，但目录里没有" % name)
            continue
        size = path.stat().st_size
        if entry.get("size") is not None and int(entry["size"]) != size:
            problems.append("%s 大小不符：manifest %s，实际 %d" % (name, entry["size"], size))
        recorded = str(entry.get("sha256", ""))
        if not recorded:
            problems.append("%s 在 manifest.json 里没有 sha256" % name)
        elif sha256_file(path) != recorded:
            problems.append("%s 的 sha256 与 manifest.json 不符" % name)
        wheel = wheelspec.parse_wheel(name)
        if wheel:
            by_key.setdefault(wheel.key, []).append(wheel)

    for path in sorted(directory.iterdir()):
        if not path.is_file() or path.name in (MANIFEST_NAME, LOCK_NAME):
            continue
        if path.name in listed:
            continue
        if wheelspec.is_sdist(path.name) or path.name.endswith(wheelspec.WHEEL_SUFFIX):
            problems.append(
                "%s 在目录里却没登记：安装器会忽略它，但它在制品集里就是隐患" % path.name
            )
        else:
            warnings.append(
                "%s 不是制品，安装器不会搬它；确认它本来就不该在制品目录里" % path.name
            )
    for key, wheels in by_key.items():
        if len(wheels) > 1:
            warnings.append(
                "%s %s 有 %d 个候选：%s"
                % (key[0], key[1], len(wheels), ", ".join(sorted(item.filename for item in wheels)))
            )
    return problems, warnings, by_key


def check_lock(directory, manifest, by_key):
    problems, warnings = [], []
    lock_path = directory / LOCK_NAME
    if not lock_path.is_file():
        return ["%s 不存在" % lock_path], warnings, []
    recorded = str(manifest.get("requirements_sha256", ""))
    if recorded and sha256_file(lock_path) != recorded:
        problems.append("requirements.txt 的 sha256 与 manifest.json 不符")

    pins = [
        line
        for line in lock_path.read_text(encoding="utf-8").splitlines()
        if line.strip() and not line.strip().startswith("#")
    ]
    parsed = [wheelspec.parse_pin(line) for line in pins]
    for line, item in zip(pins, parsed):
        if item is None:
            problems.append("锁文件里有无法解析的条目，安装器那条路解释不了它：%r" % line)
    parsed = [item for item in parsed if item]

    covered = set()
    for name, op, version in parsed:
        if name not in {key[0] for key in by_key}:
            problems.append("锁文件钉了 %s，制品集里没有同名轮子" % name)
            continue
        if version is None:
            covered.add(name)
            continue
        if op not in wheelspec.VERIFIABLE_OPS:
            warnings.append("%s 用的是 %s，本工具只核 ==，请人工确认" % (name, op))
            continue
        hits = [
            key
            for key in by_key
            if key[0] == name and wheelspec.pin_matches(op, version, key[1])
        ]
        if not hits:
            have = sorted(key[1] for key in by_key if key[0] == name)
            problems.append(
                "锁文件钉 %s%s%s，制品集里只有 %s" % (name, op, version, ", ".join(have) or "空")
            )
        else:
            covered.add(name)

    for (name, version), wheels in by_key.items():
        if name not in covered:
            warnings.append(
                "%s %s 在制品集里但没有被锁文件钉住：会被搬上节点却永远不会被安装"
                % (name, version)
            )
    return problems, warnings, parsed


def check_profile(directory, manifest, spec_path, profile_path, by_key):
    problems, warnings = [], {}
    bundle = {key: manifest.get("profile", {}).get(key) for key in PROFILE_FIELDS}
    facts = {"bundle_profile": bundle}

    if spec_path and Path(spec_path).is_file():
        try:
            spec = read_json(spec_path)
        except ValueError as error:
            problems.append(str(error))
            spec = None
        if spec:
            wanted = {key: spec.get("profile", {}).get(key) for key in PROFILE_FIELDS}
            facts["spec_profile"] = wanted
            for key in PROFILE_FIELDS:
                if not matches(wanted.get(key), bundle.get(key)):
                    problems.append(
                        "规格文件声明 %s=%s，而这份 bundle 是 %s：重跑 build.py"
                        % (key, wanted.get(key), bundle.get(key))
                    )

    image = None
    if profile_path:
        if not Path(profile_path).is_file():
            warnings.append("找不到镜像 profile %s，跳过与镜像的比对" % profile_path)
        else:
            try:
                image = read_json(profile_path)
            except ValueError as error:
                problems.append(str(error))
    if image:
        facts["image_profile"] = {key: image.get(key) for key in PROFILE_FIELDS}
        for key in PROFILE_FIELDS:
            if not matches(image.get(key), bundle.get(key)):
                problems.append(
                    "镜像 profile 的 %s=%s 与 bundle 的 %s=%s 不一致；"
                    "容器自检会判不兼容并释放实例"
                    % (key, image.get(key), key, bundle.get(key))
                )

    target = image or {}
    profile_python = target.get("python") or bundle.get("python")
    for (name, version), wheels in sorted(by_key.items()):
        for wheel in wheels:
            ok, token = python_tag_ok(wheel, profile_python)
            if not ok:
                problems.append(
                    "%s 的 python 标签是 %s，装不进 profile 要求的 python %s"
                    % (wheel.filename, token, profile_python)
                )
    sglang_versions = sorted({key[1] for key in by_key if key[0] == "sglang"})
    facts["sglang_in_bundle"] = sglang_versions
    wanted_sglang = (image or {}).get("sglang") or bundle.get("sglang")
    if wanted_sglang:
        if not sglang_versions:
            problems.append("profile 要求 sglang %s，但制品集里没有 sglang" % wanted_sglang)
        else:
            for version in sglang_versions:
                if not matches(wanted_sglang, version):
                    problems.append(
                        "profile 要求 sglang %s，制品集里是 %s" % (wanted_sglang, version)
                    )
    return problems, warnings, facts


def run_install_check(pip, directory, lock_path):
    argv = list(pip) + [
        "install",
        "--dry-run",
        "--ignore-installed",
        "--no-index",
        "--find-links",
        str(directory),
        "--requirement",
        str(lock_path),
    ]
    try:
        result = subprocess.run(argv, capture_output=True, text=True, timeout=1800)
    except (OSError, subprocess.SubprocessError) as error:
        return ["离线安装演练没跑起来：%s（%s）" % (" ".join(shlex.quote(a) for a in argv), error)]
    if result.returncode == 0:
        return []
    tail = [line for line in (result.stdout + "\n" + result.stderr).splitlines() if line.strip()]
    return ["离线安装演练失败：" + " / ".join(tail[-4:])]


def verify(directory, spec_path, profile_path, install_check, pip):
    problems, warnings, facts = [], [], {}
    manifest_path = directory / MANIFEST_NAME
    if not manifest_path.is_file():
        return ["%s 不存在：这不是一个 wheelhouse" % manifest_path], warnings, facts
    try:
        manifest = read_json(manifest_path)
    except ValueError as error:
        return [str(error)], warnings, facts
    if manifest.get("schema") != MANIFEST_SCHEMA:
        problems.append(
            "manifest.json 的 schema 是 %r，期望 %s" % (manifest.get("schema"), MANIFEST_SCHEMA)
        )

    found, warned, by_key = check_manifest(directory, manifest)
    problems += found
    warnings += warned

    found, warned, parsed = check_lock(directory, manifest, by_key)
    problems += found
    warnings += warned
    facts["pins"] = len(parsed)

    found, warned, profile_facts = check_profile(
        directory, manifest, spec_path, profile_path, by_key
    )
    problems += found
    warnings += warned
    facts.update(profile_facts)

    facts["artifacts"] = len(manifest.get("files") or [])
    facts["bytes"] = sum(
        int(item.get("size") or 0) for item in (manifest.get("files") or []) if isinstance(item, dict)
    )

    if install_check:
        lock_path = directory / LOCK_NAME
        if lock_path.is_file():
            problems += run_install_check(pip, directory, lock_path)

    return problems, warnings, facts


def human(directory, problems, warnings, facts):
    lines = ["", "wheelhouse : %s" % directory]
    bundle = facts.get("bundle_profile") or {}
    if bundle:
        lines.append(
            "bundle     : %s / cuda %s / python %s / sglang %s"
            % (bundle.get("os"), bundle.get("cuda"), bundle.get("python"), bundle.get("sglang"))
        )
    image = facts.get("image_profile") or {}
    if image:
        lines.append(
            "image      : %s / cuda %s / python %s / sglang %s"
            % (image.get("os"), image.get("cuda"), image.get("python"), image.get("sglang"))
        )
    lines.append(
        "artifacts  : %d 个，%.2f GiB；锁文件 %d 条"
        % (facts.get("artifacts", 0), facts.get("bytes", 0) / float(1 << 30), facts.get("pins", 0))
    )
    for warning in warnings:
        lines.append("警告       : %s" % warning)
    for problem in problems:
        lines.append("问题       : %s" % problem)
    lines.append("")
    lines.append("结论       : %s" % ("通过" if not problems else "%d 处问题" % len(problems)))
    lines.append("")
    return "\n".join(lines)


def parse_args(argv):
    parser = argparse.ArgumentParser(
        description="核对一份 wheelhouse 是否满足消费端的全部闸门",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument(
        "--wheelhouse",
        default=str(Path(__file__).with_name("out")),
        help="要核的 wheelhouse 目录，默认 <本目录>/out",
    )
    parser.add_argument(
        "--spec", default=str(Path(__file__).with_name(SPEC_NAME)), help="wheelhouse 规格文件"
    )
    parser.add_argument(
        "--profile",
        default="",
        help="镜像 profile.json，默认仓库里的 deploy/container/profile.json",
    )
    parser.add_argument(
        "--install-check",
        action="store_true",
        help="额外跑一次 pip install --dry-run --no-index 的离线解析演练",
    )
    parser.add_argument(
        "--pip",
        default="",
        help="执行 pip 的命令，按 shell 规则切分（Windows 路径请用正斜杠）",
    )
    parser.add_argument("--strict", action="store_true", help="把警告也当失败")
    parser.add_argument("--json", action="store_true", help="在 stdout 上输出机器可读结论")
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(sys.argv[1:] if argv is None else argv)
    directory = Path(args.wheelhouse).resolve()
    profile_path = args.profile or str(
        Path(__file__).resolve().parents[2] / "deploy" / "container" / "profile.json"
    )
    pip = shlex.split(args.pip) if args.pip else [sys.executable, "-m", "pip"]

    if not directory.is_dir():
        print("看这里：%s 不是一个目录" % directory, file=sys.stderr)
        return 1

    problems, warnings, facts = verify(
        directory, args.spec, profile_path, args.install_check, pip
    )
    if args.strict:
        problems = problems + warnings
        warnings = []
    if args.json:
        print(
            json.dumps(
                {
                    "wheelhouse": str(directory),
                    "ok": not problems,
                    "problems": problems,
                    "warnings": warnings,
                    "facts": facts,
                },
                ensure_ascii=False,
            )
        )
    else:
        print(human(directory, problems, warnings, facts))
        print("下一步：按 provision/wheelhouse/README.md 里的投送方式搬运。")
    return 0 if not problems else 1


if __name__ == "__main__":
    sys.exit(main())
