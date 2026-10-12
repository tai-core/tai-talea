#!/usr/bin/env python3
"""Freeze a networked reference environment into an offline wheelhouse.

The Talea provisioning path only consumes artifacts. ``provision/ssh_install.py``
refuses anything that is not a ``<basename>.whl``, uploads it only after the
SHA256 recorded in ``manifest.json`` matches, and the node then runs
``pip install --no-index --find-links=<wheelhouse>``. Nothing in that path
resolves, downloads or compiles anything, which is what lets a fleet come up on
a base image with no package index in reach.

This script is the missing other half: it produces exactly that directory from
an environment that has been built once, with network.

    ./build-env.sh /root/ref-venv                       # once, with network
    ./build.py --venv /root/ref-venv --output ../wheelhouse-out
    ./verify.py --wheelhouse ../wheelhouse-out --profile ../../deploy/container/profile.json

Design notes worth keeping:

* The lock comes from ``pip freeze --all`` of the reference environment, so the
  full transitive closure is captured without a resolver.
* Downloads are always ``--no-deps`` against that closure. Adding a resolver
  here can only introduce a version the reference environment never had.
* A wheel already present in the output directory wins over the index. That is
  how a locally built wheel (an internal SGLang fork, a package whose sdist
  needs a submodule the index does not ship) takes precedence.
* Anything that ends up as an sdist is turned into a wheel, because the
  installer will reject it otherwise.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import platform
import shlex
import shutil
import subprocess
import sys
import time
from pathlib import Path

import wheelspec

SPEC_NAME = "spec.json"
LOCK_NAME = "requirements.txt"
MANIFEST_NAME = "manifest.json"
MANIFEST_SCHEMA = "talea-wheelhouse/1"
SPEC_SCHEMA = "talea-wheelhouse-spec/1"

GIB = float(1 << 30)


class BuildError(RuntimeError):
    pass


def log(message=""):
    print(message, file=sys.stderr, flush=True)


def sha256_file(path, chunk=4 << 20):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(chunk), b""):
            digest.update(block)
    return digest.hexdigest()


def preview(argv):
    return " ".join(shlex.quote(str(item)) for item in argv)


# --------------------------------------------------------------------------- #
# specification
# --------------------------------------------------------------------------- #


def load_spec(path):
    try:
        raw = Path(path).read_text(encoding="utf-8")
    except OSError as error:
        raise BuildError("规格文件不可读：%s" % error)
    try:
        spec = json.loads(raw)
    except ValueError as error:
        raise BuildError("规格文件不是合法 JSON：%s" % error)
    if spec.get("schema") != SPEC_SCHEMA:
        raise BuildError("规格文件 schema 必须是 %s" % SPEC_SCHEMA)
    for field in ("profile", "indexes", "requirements"):
        if not isinstance(spec.get(field), dict):
            raise BuildError("规格文件缺少 %s 段" % field)
    profile = {key: value for key, value in spec["profile"].items() if key != "comment"}
    for field in ("os", "cuda", "python", "sglang", "wheelhouse", "virtualenv"):
        if not str(profile.get(field, "")).strip():
            raise BuildError("规格文件 profile 缺少 %s" % field)
    spec["profile"] = profile
    return spec


def spec_directory(path):
    return Path(path).resolve().parent


# --------------------------------------------------------------------------- #
# pip plumbing
# --------------------------------------------------------------------------- #


def venv_pip(venv):
    for candidate in (Path(venv) / "bin" / "pip", Path(venv) / "Scripts" / "pip.exe"):
        if candidate.exists():
            return [str(candidate)]
    return None


def pip_command(args):
    if args.pip:
        command = shlex.split(args.pip)
        if not command:
            raise BuildError("--pip 是空的")
        return command
    if args.venv:
        command = venv_pip(args.venv)
        if command is None:
            raise BuildError("--venv 里没有 pip：%s" % args.venv)
        return command
    return [sys.executable, "-m", "pip"]


def index_flags(spec):
    flags = []
    indexes = spec.get("indexes") or {}
    primary = str(indexes.get("primary") or "").strip()
    if primary:
        flags += ["--index-url", primary]
    for extra in indexes.get("extra") or []:
        if str(extra).strip():
            flags += ["--extra-index-url", str(extra).strip()]
    return flags


def cross_flags(spec, args):
    if not args.cross:
        return []
    cross = (spec.get("builder") or {}).get("cross") or {}
    flags = []
    for item in cross.get("platforms") or []:
        flags += ["--platform", str(item)]
    abi = str(cross.get("abi") or "").strip()
    if abi:
        flags += ["--abi", abi]
    flags += ["--python-version", str(spec["profile"]["python"])]
    return flags


def pip_version(pip):
    try:
        result = subprocess.run(
            list(pip) + ["--version"], capture_output=True, text=True, timeout=60
        )
    except (OSError, subprocess.SubprocessError):
        return ""
    return result.stdout.strip()


def run_quiet(argv, tolerate_error=True):
    """Run pip without letting its chatter land in the JSON summary."""
    log("+ " + preview(argv))
    try:
        result = subprocess.run(argv, capture_output=True, text=True)
    except OSError as error:
        if not tolerate_error:
            raise BuildError("无法执行：%s（%s）" % (preview(argv), error))
        log("  执行失败：%s" % error)
        return 1, ""
    if result.returncode:
        tail = [line for line in result.stdout.splitlines() if line.strip()][-6:]
        for line in tail:
            log("  | " + line)
        for line in [line for line in result.stderr.splitlines() if line.strip()][-6:]:
            log("  | " + line)
    return result.returncode, result.stdout


# --------------------------------------------------------------------------- #
# lock resolution
# --------------------------------------------------------------------------- #


def freeze(pip):
    log("+ %s" % preview(list(pip) + ["freeze", "--all"]))
    result = subprocess.run(list(pip) + ["freeze", "--all"], capture_output=True, text=True)
    if result.returncode:
        raise BuildError(
            "参考环境的 pip freeze 失败：%s" % (result.stderr.strip() or result.returncode)
        )
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def read_lock(path):
    try:
        raw = Path(path).read_text(encoding="utf-8")
    except OSError as error:
        raise BuildError("锁文件不可读：%s" % error)
    return [line.strip() for line in raw.splitlines() if line.strip()]


def apply_excludes(lock, excluded):
    names = {wheelspec.canonical(name) for name in excluded or []}
    if not names:
        return lock, []
    kept, dropped = [], []
    for line in lock:
        parsed = wheelspec.parse_pin(line)
        if parsed and parsed[0] in names:
            dropped.append(line)
        else:
            kept.append(line)
    return kept, dropped


def apply_strip_local(lock):
    """Normalise a lock to version pins without local segments.

    Only coherent as a paired operation: the lock and the download use the same
    stripped pin. Off by default, because stripping turns ``torch==2.13.0+cu130``
    into ``torch==2.13.0``, and PyPI serves a different build under that name.
    """
    replaced, kept = {}, []
    for line in lock:
        stripped = wheelspec.strip_local(line)
        if stripped and stripped != line:
            kept.append(stripped)
            replaced[line] = stripped
        else:
            kept.append(line)
    return kept, replaced


def check_top_contract(lock, top):
    """Assert the reference environment really is the one we think it is."""
    parsed_lock = [parsed for parsed in (wheelspec.parse_pin(line) for line in lock) if parsed]
    problems = []
    for entry in top or []:
        wanted = wheelspec.parse_pin(entry)
        if wanted is None:
            problems.append("规格文件 requirements.top 里有无法解析的条目：%r" % entry)
            continue
        name, op, version = wanted
        found = [item for item in parsed_lock if item[0] == name]
        if not found:
            problems.append("参考环境里没有 %s（锁文件 %d 条）" % (entry, len(lock)))
            continue
        if op and version:
            if not any(wheelspec.pin_matches("==", version, item[2] or "") for item in found):
                observed = ", ".join(sorted({item[2] or "?" for item in found}))
                problems.append("参考环境装的是 %s，规格要求 %s" % (observed, entry))
    return problems


# --------------------------------------------------------------------------- #
# artifact selection
# --------------------------------------------------------------------------- #


def index_output(directory, include_sdists=False):
    """Map ``(canonical name, version)`` to file names already in ``directory``.

    Source archives are included only while a pin is being fetched: they are
    what a source-only pin resolves to, and ``convert_sdists`` turns them into
    wheels afterwards.
    """
    found = {}
    for path in sorted(directory.iterdir()):
        if not path.is_file():
            continue
        wheel = wheelspec.parse_wheel(path.name)
        if wheel:
            found.setdefault(wheel.key, []).append(path.name)
            continue
        if include_sdists:
            sdist = wheelspec.parse_sdist(path.name)
            if sdist:
                found.setdefault(sdist, []).append(path.name)
    return found


def find_artifact(present, pin):
    parsed = wheelspec.parse_pin(pin)
    if parsed is None:
        return None
    name, op, version = parsed
    if version is None:
        for (candidate, _), names in present.items():
            if candidate == name:
                return names[0]
        return None
    for (candidate, have), names in present.items():
        if candidate != name:
            continue
        if wheelspec.pin_matches(op or "==", version, have):
            return names[0]
    return None


def copy_local_wheels(spec, args, output):
    """Lay down locally built wheels first: an existing artifact wins.

    Two sources with different rules. ``local_wheels.paths`` is wherever the
    operator points it (a NAS, an artifact store) and is not audited.
    ``local_wheels.search`` is relative to the spec, i.e. inside the
    repository, so it has to survive ``git push``: no name the consumer would
    reject, and nothing over the hosting platform's per-file limit.
    """
    base = spec_directory(args.spec)
    sources = []
    for entry in (spec.get("local_wheels") or {}).get("paths") or []:
        for match in sorted(base.glob(str(entry))):
            sources.append(match)
    for entry in (spec.get("local_wheels") or {}).get("search") or []:
        for match in sorted(base.glob(str(entry))):
            if match.is_dir():
                problems, sized = wheelspec.check_local_slot(match)
                if problems:
                    raise BuildError(
                        "本地轮子槽 %s 有问题：\n  - %s"
                        % (match, "\n  - ".join(problems))
                    )
                if sized:
                    log(
                        "本地槽 %s：%d 个轮子，共 %.1f MiB"
                        % (match, len(sized), sum(item[1] for item in sized) / (1 << 20))
                    )
                sources.extend(sorted(match.glob("*" + wheelspec.WHEEL_SUFFIX)))
            elif match.is_file():
                sources.append(match)
    copied, skipped = [], []
    for source in sources:
        if not source.is_file():
            skipped.append("%s（不是文件）" % source)
            continue
        if not wheelspec.parse_wheel(source.name):
            raise BuildError(
                "local_wheels 里的 %s 不是合法的轮子文件名；"
                "install_node.py 只接受 <name>-<version>-<py>-<abi>-<plat>.whl" % source.name
            )
        destination = output / source.name
        if not destination.exists() or sha256_file(destination) != sha256_file(source):
            shutil.copy2(source, destination)
        copied.append(source.name)
    return copied, skipped


def download(lock, spec, args, pip, output):
    """Fetch every pin with ``--no-deps``; the lock already is the closure."""
    base = list(pip) + [
        "download",
        "--no-deps",
        "--no-cache-dir",
        "--disable-pip-version-check",
        "--dest",
        str(output),
    ]
    base += index_flags(spec)
    base += cross_flags(spec, args)
    if args.cross:
        base += ["--only-binary", ":all:"]

    present = index_output(output, include_sdists=True)
    results, pending = {}, []
    for pin in lock:
        found = find_artifact(present, pin)
        if found:
            results[pin] = {"file": found, "source": "local"}
        else:
            pending.append(pin)
    if not pending:
        return results, []

    log("下载 %d 个钉（--no-deps，锁文件即闭包）" % len(pending))
    run_quiet(base + pending)
    present = index_output(output, include_sdists=True)

    unresolved = []
    for pin in pending:
        found = find_artifact(present, pin)
        if found:
            results[pin] = {"file": found, "source": "index"}
            continue
        run_quiet(base + [pin])
        present = index_output(output, include_sdists=True)
        found = find_artifact(present, pin)
        if found:
            results[pin] = {"file": found, "source": "index"}
            continue
        unresolved.append(pin)
    return results, unresolved


def sdist_name(filename):
    parsed = wheelspec.parse_sdist(filename)
    return parsed[0] if parsed else ""


def convert_sdists(spec, args, pip, output):
    """Turn downloaded source packages into wheels; the installer takes no sdist."""
    archives = [
        path
        for path in sorted(output.iterdir())
        if path.is_file() and wheelspec.is_sdist(path.name)
    ]
    if not archives:
        return [], []
    if args.cross:
        return [], [path.name for path in archives]
    if not (spec.get("builder") or {}).get("build_sdists", True):
        return [], [path.name for path in archives]
    built, failed = [], []
    for archive in archives:
        argv = (
            list(pip)
            + [
                "wheel",
                "--no-deps",
                "--no-cache-dir",
                "--wheel-dir",
                str(output),
            ]
            + index_flags(spec)
            + [str(archive)]
        )
        run_quiet(argv)
        wanted = sdist_name(archive.name)
        landed = []
        for path in sorted(output.glob("*" + wheelspec.WHEEL_SUFFIX)):
            wheel = wheelspec.parse_wheel(path.name)
            if wheel and wanted and wheelspec.canonical(wheel.name) == wanted:
                landed.append(path)
        if landed:
            archive.unlink()
            built.append(landed[0].name)
            log("  %s -> %s" % (archive.name, landed[0].name))
        else:
            failed.append(archive.name)
    return built, failed


# --------------------------------------------------------------------------- #
# writing the bundle
# --------------------------------------------------------------------------- #


def write_lock(output, lock):
    (output / LOCK_NAME).write_text("\n".join(lock) + "\n", encoding="utf-8")


def write_manifest(output, spec, spec_path, lock, builder, notes):
    entries = []
    for path in sorted(output.iterdir()):
        if not path.is_file() or path.name in (MANIFEST_NAME, LOCK_NAME):
            continue
        entries.append(
            {"name": path.name, "size": path.stat().st_size, "sha256": sha256_file(path)}
        )
    manifest = {
        "schema": MANIFEST_SCHEMA,
        "comment": [
            "The installer reads two things from here: ``files`` with its",
            "per-file sha256, and nothing else. The remaining keys are",
            "provenance for whoever has to answer 'what is in this bundle'",
            "six months from now. manifest.json and requirements.txt are",
            "deliberately not listed in ``files``.",
        ],
        "profile": spec["profile"],
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "builder": {
            "host": platform.node(),
            "python": platform.python_version(),
            "pip": pip_version(builder["pip"]),
            "spec": str(spec_path),
            "spec_sha256": sha256_file(spec_path),
            "cross": bool(builder["cross"]),
        },
        "requirements_count": len(lock),
        "requirements_sha256": sha256_file(output / LOCK_NAME),
        "artifact_sources": notes,
        "files": entries,
    }
    (output / MANIFEST_NAME).write_text(
        json.dumps(manifest, indent=1, ensure_ascii=False, sort_keys=False) + "\n",
        encoding="utf-8",
    )
    return manifest


def summarize(output, manifest, spec, resolution):
    artifacts = manifest["files"]
    total = sum(item["size"] for item in artifacts) / GIB
    local = sorted(name for name, source in resolution.items() if source == "local")
    lines = [
        "",
        "wheelhouse : %s" % output,
        "profile    : %s / cuda %s / python %s / sglang %s"
        % (
            spec["profile"]["os"],
            spec["profile"]["cuda"],
            spec["profile"]["python"],
            spec["profile"]["sglang"],
        ),
        "artifacts  : %d 个轮子，%.2f GiB" % (len(artifacts), total),
        "lock       : %d 条 -> %s" % (manifest["requirements_count"], LOCK_NAME),
        "本地覆盖   : %d 个" % len(local),
        "manifest   : %s" % MANIFEST_NAME,
        "",
        "把下面三个值填进控制节点的安装清单（onboarding manifest）：",
        '  "wheelhouse_source"     : "%s"' % output,
        '  "wheel_manifest_sha256" : "%s"' % sha256_file(output / MANIFEST_NAME),
        '  "requirements_sha256"   : "%s"' % manifest["requirements_sha256"],
        "",
        "接着核一遍再搬运：",
        "  %s --wheelhouse %s --profile %s"
        % (
            Path(__file__).with_name("verify.py"),
            output,
            Path(__file__).resolve().parents[2] / "deploy" / "container" / "profile.json",
        ),
        "",
    ]
    over = [item for item in artifacts if item["size"] > wheelspec.GIT_FILE_LIMIT]
    if over:
        lines.insert(
            7,
            "不可提交   : %d 个轮子超过 git 单文件 %.0f MiB 上限（最大 %.0f MiB），"
            "制品只能放制品库"
            % (
                len(over),
                wheelspec.GIT_FILE_LIMIT / (1 << 20),
                max(item["size"] for item in over) / (1 << 20),
            ),
        )
    log("\n".join(lines))


# --------------------------------------------------------------------------- #
# entry point
# --------------------------------------------------------------------------- #


def parse_args(argv):
    parser = argparse.ArgumentParser(
        description="把参考环境冻结成可搬运的离线 wheelhouse",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument("--spec", default=str(Path(__file__).with_name(SPEC_NAME)))
    parser.add_argument(
        "--output",
        default="",
        help="输出目录，默认 <spec 同级>/out",
    )
    parser.add_argument(
        "--venv",
        default="",
        help="参考环境：用它自己的 pip，并把 pip freeze --all 当作锁文件",
    )
    parser.add_argument(
        "--requirements",
        default="",
        help="改用一份已冻结的锁文件，而不是从参考环境现冻",
    )
    parser.add_argument(
        "--pip",
        default="",
        help='执行 pip 的命令，按 shell 规则切分，例如 --pip "python3.12 -m pip"'
        "（Windows 路径请用正斜杠，反斜杠会被 shell 规则当转义吃掉）",
    )
    parser.add_argument(
        "--cross",
        action="store_true",
        help="在非目标平台上交叉下载（只接受轮子，且不现场编译 sdist）",
    )
    parser.add_argument(
        "--clean",
        action="store_true",
        help="先删掉输出目录里本工具拥有的旧制品（轮子、sdist、manifest、requirements）",
    )
    parser.add_argument("--json", action="store_true", help="在 stdout 上输出机器可读摘要")
    return parser.parse_args(argv)


def build(args):
    spec_path = Path(args.spec).resolve()
    spec = load_spec(spec_path)
    output = Path(args.output).resolve() if args.output else spec_directory(spec_path) / "out"
    pip = pip_command(args)

    if not args.venv and not args.requirements:
        raise BuildError(
            "要么给 --venv（推荐：锁文件来自参考环境的 pip freeze --all），"
            "要么给 --requirements；只有 requirements.top 是造不出闭包的"
        )
    if args.cross and not args.venv and not args.requirements:
        raise BuildError("--cross 也需要一份锁文件")

    profile = spec["profile"]
    log("规格   : %s" % spec_path)
    log("输出   : %s" % output)
    log("profile: %s / cuda %s / python %s / sglang %s"
        % (profile["os"], profile["cuda"], profile["python"], profile["sglang"]))
    if not args.cross:
        observed = "%d.%d" % (sys.version_info.major, sys.version_info.minor)
        if observed != str(profile["python"]):
            log(
                "提示   : 本机 Python 是 %s，profile 要求 %s；原生下载需要两者的"
                " wheel 标签一致，跨机器请用 --cross" % (observed, profile["python"])
            )

    output.mkdir(parents=True, exist_ok=True)
    if args.clean:
        for path in sorted(output.iterdir()):
            if not path.is_file():
                continue
            if path.name in (MANIFEST_NAME, LOCK_NAME) or wheelspec.parse_wheel(path.name):
                path.unlink()
            elif wheelspec.is_sdist(path.name):
                path.unlink()

    local_copied, local_skipped = copy_local_wheels(spec, args, output)
    if local_copied:
        log("本地轮子: %d 个（命中即不再从索引下载）" % len(local_copied))

    lock = freeze(pip) if args.venv else read_lock(args.requirements)
    lock, dropped = apply_excludes(lock, (spec.get("requirements") or {}).get("exclude"))
    if dropped:
        log("按规格排除 %d 条：%s" % (len(dropped), ", ".join(dropped)))

    problems = check_top_contract(lock, (spec.get("requirements") or {}).get("top"))
    if problems:
        for problem in problems:
            log("合同   : %s" % problem)
        raise BuildError("参考环境与规格不符；先确认这台机器就是参考环境（见 build-env.sh）")

    if (spec.get("builder") or {}).get("strip_local_version"):
        lock, replaced = apply_strip_local(lock)
        if replaced:
            log("按规格去掉 %d 条本地版本标签" % len(replaced))

    results, unresolved = download(lock, spec, args, pip, output)
    if unresolved:
        for pin in unresolved:
            log("未解析 : %s" % pin)
        raise BuildError(
            "%d 个钉既不在本地轮子里，也下不到。把对应的轮子放进 "
            "provision/wheelhouse/wheels/，或在规格的 indexes.extra 里补上能提供它的索引。"
            "带本地版本标签（torch==2.13.0+cu130、sglang==0.0.0+g<sha>.cu12）的钉子"
            "一定要走这条路：去掉标签会让公共索引悄悄换成另一份构建，"
            "所以这里不做自动降级" % len(unresolved)
        )

    built, failed = convert_sdists(spec, args, pip, output)
    if built:
        log("sdist  : %d 个已现场编译成轮子" % len(built))
    if failed:
        raise BuildError(
            "这些源码包没能变成轮子：%s。install_node.py 只接受 .whl，"
            "请在参考环境里对它们跑一次 pip wheel，把产出的轮子放进 "
            "provision/wheelhouse/wheels/ 再重跑" % ", ".join(failed)
        )

    # Final completeness check: a pin satisfied by a source archive a moment ago
    # is only satisfied by a wheel now, so this is the check that matters.
    wheels_only = index_output(output)
    missing = [pin for pin in lock if not find_artifact(wheels_only, pin)]
    if missing:
        raise BuildError(
            "这些钉在制品集里没有对应轮子：%s" % ", ".join(missing)
        )

    strays = []
    for path in sorted(output.iterdir()):
        if not path.is_file() or path.name in (MANIFEST_NAME, LOCK_NAME):
            continue
        if wheelspec.is_sdist(path.name):
            strays.append(path.name)
        elif path.name.endswith(wheelspec.WHEEL_SUFFIX) and not wheelspec.parse_wheel(path.name):
            strays.append(path.name)
        else:
            log("说明   : %s 留在目录里但不在制品集内，安装器不会搬它" % path.name)
    if strays:
        raise BuildError(
            "输出目录里有 %d 个成品形态不对的文件：%s；"
            "消费端要么拒收（sdist）要么解析不了（损坏的轮子文件名）"
            % (len(strays), ", ".join(strays))
        )

    write_lock(output, lock)
    resolution = {}
    for record in results.values():
        resolution[record["file"]] = record["source"]
    for name in local_copied:
        resolution[name] = "local"
    for name in built:
        resolution[name] = "sdist"
    notes = {
        "counts": {
            "local": sum(1 for value in resolution.values() if value == "local"),
            "index": sum(1 for value in resolution.values() if value == "index"),
            "sdist": sum(1 for value in resolution.values() if value == "sdist"),
        },
        "local_wheels": sorted(local_copied),
        "converted_sdists": sorted(built),
        "skipped_local": sorted(local_skipped),
    }
    manifest = write_manifest(
        output,
        spec,
        spec_path,
        lock,
        {"pip": pip, "cross": args.cross},
        notes,
    )

    if args.json:
        print(
            json.dumps(
                {
                    "wheelhouse": str(output),
                    "artifacts": len(manifest["files"]),
                    "bytes": sum(item["size"] for item in manifest["files"]),
                    "requirements_sha256": manifest["requirements_sha256"],
                    "wheel_manifest_sha256": sha256_file(output / MANIFEST_NAME),
                    "notes": notes,
                },
                ensure_ascii=False,
            )
        )
    else:
        summarize(output, manifest, spec, resolution)
    return 0


def main(argv=None):
    args = parse_args(sys.argv[1:] if argv is None else argv)
    try:
        return build(args)
    except BuildError as error:
        log("")
        log("失败：%s" % error)
        return 1
    except KeyboardInterrupt:  # pragma: no cover
        return 130


if __name__ == "__main__":
    sys.exit(main())
