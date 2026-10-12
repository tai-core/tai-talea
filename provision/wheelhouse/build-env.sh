#!/usr/bin/env bash
#
# Build the Talea reference runtime environment from scratch. Needs network.
#
# This is the networked half of the provisioning story. It produces one
# virtualenv that actually serves GLM-5.3-Flash on H100, which build.py then
# freezes into an offline wheelhouse. Everything the node later does offline is
# decided here, so the pins below are the real contract: they were validated on
# 8xH100 (see docs/glm53-h100-validation-2026-09-23.md) and every one of them
# is load bearing.
#
#   ./build-env.sh /root/talea-ref
#   ./build.py --venv /root/talea-ref --output ../out
#   ./verify.py --wheelhouse ../out
#
# Deliberate omissions, each of which was a live incident:
#
#   * ninja        - not in the base image. SGLang's tilelang and EAGLE paths
#                    shell out to it and die with FileNotFoundError before any
#                    kernel compiles. Installed from apt *and* kept in the
#                    wheelhouse, because a platform with no package source can
#                    only use the wheel.
#   * deep_gemm    - PyPI only carries a 1.0.0 sdist whose build needs a cutlass
#                    submodule that is not in the archive. sglang 0.5.20 ships
#                    sgl-deep-gemm 0.2.0, which is what --moe-runner-backend
#                    deep_gemm actually resolves to.
#   * nvidia/*     - the CUDA userspace toolkit comes from these wheels, not
#                    from the image, so the JIT links against a known version
#                    no matter what the base image carries.
#
set -euo pipefail

VENV="${HOME}/talea-ref-venv"
PYTHON="python3"
INDEX="https://mirrors.bfsu.edu.cn/pypi/web/simple"
# Three extra indexes, and each is load-bearing:
#   pytorch  -- torch==2.13.0+cu130 does not exist on PyPI under that name, and
#               the same-named PyPI build is a different CUDA runtime.
#   nvidia   -- sglang 0.5.20 pins cuda-tile==1.6.0rc5, which PyPI carries only
#               as a 7 KB "wheel-stub" sdist whose build backend downloads the
#               real wheel from here (pypi.nvidia.com 301s to pypi.nvidia.cn).
#               Without this index the whole install dies with
#               "No matching distribution found for cuda-tile==1.6.0rc5".
#   tencent  -- fallback mirror.
EXTRA_INDEX=(
  "https://mirrors.cloud.tencent.com/pypi/simple"
  "https://download.pytorch.org/whl/cu130"
  "https://pypi.nvidia.com/"
)
INSTALL_APT=1
WRITE_PIP_CONF=0

# Pinned reference combination. profile.python in ../wheelhouse/spec.json must
# match the interpreter these wheels are built for.
TOOLCHAIN_VERSION="13.2.86"
TORCH="torch==2.13.0"
SGLANG="sglang==0.5.20"
FLASHINFER="flashinfer-python==0.6.18"
TILELANG="tilelang==0.1.12"
MOONCAKE="mooncake-transfer-engine"
CU12_RUNTIME="nvidia-cuda-runtime-cu12==12.9.79"
RUNSTREAMER="runai-model-streamer"

usage() {
  cat <<'USAGE'
用法：build-env.sh [选项] [venv 目录]

  --python NAME       建 venv 用的解释器（默认 python3，必须等于 profile.python）
  --index URL         PyPI 主源（默认 bfsu；实测 73MB/s，tuna 只有 324KB/s）
  --extra-index URL   追加索引，可重复（pytorch 索引必须留着，torch 的 cu130
                      构建只在那里）
  --no-apt            跳过 apt（已经在有 ninja-build / python3-venv 的镜像里时）
  --write-pip-conf    额外把主源写进 /etc/pip.conf（需要 root）
  -h, --help          显示这段
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --python) PYTHON="$2"; shift 2 ;;
    --index) INDEX="$2"; shift 2 ;;
    --extra-index) EXTRA_INDEX+=("$2"); shift 2 ;;
    --no-apt) INSTALL_APT=0; shift ;;
    --write-pip-conf) WRITE_PIP_CONF=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "未知选项：$1" >&2; usage >&2; exit 2 ;;
    *) VENV="$1"; shift ;;
  esac
done

PIP_FLAGS=("--index-url" "$INDEX")
for item in "${EXTRA_INDEX[@]}"; do
  PIP_FLAGS+=("--extra-index-url" "$item")
done

echo "== 0. 目标解释器 =="
"$PYTHON" - <<'PY'
import sys
print("python %d.%d.%d" % sys.version_info[:3])
if sys.version_info[:2] != (3, 12):
    print("提示：参考组合的轮子是 cp312；次版本不同请同步改 spec.json 的 builder.cross.abi")
PY

if [ "$INSTALL_APT" = 1 ]; then
  echo "== 1. 系统依赖 =="
  # ninja-build 是必需的：缺它 sglang 的 JIT 直接崩在 FileNotFoundError 上。
  sudo apt-get update -qq || apt-get update -qq
  sudo apt-get install -y -qq python3-venv ninja-build curl ca-certificates \
    || apt-get install -y -qq python3-venv ninja-build curl ca-certificates
else
  echo "== 1. 跳过 apt（--no-apt）=="
fi

if [ "$WRITE_PIP_CONF" = 1 ]; then
  echo "== 1b. /etc/pip.conf =="
  mkdir -p /etc
  {
    echo "[global]"
    echo "index-url = $INDEX"
    for item in "${EXTRA_INDEX[@]}"; do
      echo "extra-index-url = $item"
    done
  } | tee /etc/pip.conf >/dev/null
fi

echo "== 2. 虚拟环境 =="
"$PYTHON" -m venv "$VENV"
PIP="$VENV/bin/pip"
[ -x "$PIP" ] || PIP="$VENV/Scripts/pip.exe"
"$PIP" install --no-cache-dir "${PIP_FLAGS[@]}" -U pip wheel setuptools

echo "== 3. CUDA 用户态工具链 $TOOLCHAIN_VERSION + ninja =="
"$PIP" install --no-cache-dir "${PIP_FLAGS[@]}" \
  "nvidia-cuda-nvcc==$TOOLCHAIN_VERSION" \
  "nvidia-cuda-runtime==$TOOLCHAIN_VERSION" \
  "nvidia-cuda-crt==$TOOLCHAIN_VERSION" \
  "nvidia-cuda-cccl==$TOOLCHAIN_VERSION" \
  "nvidia-nvjitlink==$TOOLCHAIN_VERSION" \
  "nvidia-nvvm==$TOOLCHAIN_VERSION" \
  ninja

echo "== 4. torch =="
"$PIP" install --no-cache-dir "${PIP_FLAGS[@]}" "$TORCH"

echo "== 5. sglang =="
"$PIP" install --no-cache-dir "${PIP_FLAGS[@]}" "$SGLANG"

echo "== 6. 钉住已验证的推理运行时 =="
"$PIP" install --no-cache-dir --no-deps --force-reinstall "${PIP_FLAGS[@]}" \
  "$FLASHINFER" "$TILELANG"

echo "== 7. PD 传输引擎 =="
"$PIP" install --no-cache-dir "${PIP_FLAGS[@]}" "$MOONCAKE"
# mooncake 的公开轮子是按 cu12 构建的，在 CUDA 13 镜像上 import 会缺
# libcudart.so.12。把 cu12 runtime 一起带上，install_node.py 会把它的 lib
# 目录加进 LD_LIBRARY_PATH。
"$PIP" install --no-cache-dir --no-deps "${PIP_FLAGS[@]}" "$CU12_RUNTIME"

echo "== 7b. 权重流式加载器 =="
# --load-format runai_streamer 是部署配置里一直在用的加载方式（NAS 上 306GB
# 权重从 50 分钟降到 6 分钟）。它在 sglang 之外，必须显式装——而且必须装在
# 参考环境里：2026-09-30 那次是事后手工往 wheelhouse 里补 runai-model-streamer
# 和 humanize 两个轮子，再重算两个哈希、再改控制节点的安装清单。把参考环境补
# 全，这一整类补丁就没了。
"$PIP" install --no-cache-dir "${PIP_FLAGS[@]}" "$RUNSTREAMER"

echo "== 8. 自检：能 import 才算数 =="
"$VENV/bin/python" - <<'PY'
import sys
import torch, sglang, sgl_kernel
print("torch", torch.__version__, "cuda", torch.version.cuda)
print("sglang", sglang.__version__)
import flashinfer, tilelang, runai_model_streamer
print("srt 运行时与流式加载器齐了")
from mooncake.engine import TransferEngine  # noqa: F401
print("mooncake 传输引擎可导入")
assert torch.cuda.is_available() or not sys.platform.startswith("linux"), "这台机器看不到 GPU"
PY

echo "== 9. 工具链目录布局（lib64 / 无版本号符号链接）=="
# sglang 的 JIT 会 -lcudart、-lnvJitLink，镜像里的工具链布局和 pip 轮子不一样；
# 少了这些链接，编译报的是「找不到库」而不是「库版本不对」，很难查。
SITE="$VENV/lib/python3.12/site-packages"
for CUDA_ROOT in "$SITE/nvidia/cu13" "$SITE/nvidia"; do
  [ -d "$CUDA_ROOT/lib" ] || continue
  [ -e "$CUDA_ROOT/lib64" ] || ln -s lib "$CUDA_ROOT/lib64"
  for so in "$CUDA_ROOT"/lib/lib*.so.*; do
    [ -e "$so" ] || continue
    link="${so%.so.*}.so"
    [ -e "$link" ] || ln -s "$(basename "$so")" "$link"
  done
done

echo
echo "参考环境就绪：$VENV"
"$VENV/bin/pip" freeze --all | wc -l | xargs echo "  依赖条数："
echo "  下一步：./build.py --venv $VENV --output ../out"
