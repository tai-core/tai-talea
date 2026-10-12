#!/usr/bin/env bash
#
# Prepare /opt/tai-talea/{venv,wheelhouse} inside a worker image.
#
# Runs during `docker build`. Two shapes are legal and this script covers both:
#
#   * the base image already carries a verified runtime  -> nothing to do, but
#     the postconditions are still asserted, so a broken base image fails the
#     build instead of failing a container twelve minutes into a rollout;
#   * a wheelhouse was supplied via --build-arg WHEELHOUSE_DIR -> install the
#     locked runtime from it with no package index in reach.
#
# The wheelhouse is not assembled by hand: it comes from
# provision/wheelhouse (build-env.sh -> build.py -> verify.py).
#
set -euo pipefail

ROOT="${TAI_TALEA_ROOT:-/opt/tai-talea}"
PYTHON="${TALEA_PYTHON:-3.12}"
VENV="${ROOT}/venv"
WHEELHOUSE="${ROOT}/wheelhouse"
SITE="${VENV}/lib/python${PYTHON}/site-packages"

echo "talea-runtime: root=${ROOT} python=${PYTHON}"

# --- runtime layer, only when it is ours to build --------------------------
if [ -f "${WHEELHOUSE}/requirements.txt" ] && [ ! -x "${VENV}/bin/python" ]; then
  echo "talea-runtime: installing the locked runtime from the supplied wheelhouse"
  apt-get update -qq
  # ninja-build: SGLang's tilelang and EAGLE paths shell out to ninja and die
  # with a bare FileNotFoundError when it is missing. libnuma/libibverbs/
  # rdma-core are what the tensor-parallel and RDMA paths link against.
  apt-get install -y -qq python3-venv ninja-build libnuma1 libibverbs1 \
    libgomp1 rdma-core ca-certificates
  rm -rf /var/lib/apt/lists/*
  python3 -m venv "${VENV}"
  "${VENV}/bin/python" -m pip install \
    --no-index --find-links="${WHEELHOUSE}" \
    -r "${WHEELHOUSE}/requirements.txt"
  "${VENV}/bin/python" -m pip check
fi

# --- postconditions --------------------------------------------------------
[ -x "${VENV}/bin/python" ] || {
  echo "talea-runtime: no runtime at ${VENV} and no wheelhouse to build one from" >&2
  exit 1
}
[ -f "${WHEELHOUSE}/requirements.txt" ] || {
  echo "talea-runtime: ${WHEELHOUSE}/requirements.txt is missing" >&2
  exit 1
}

# --- the cu12 runtime that mooncake links against --------------------------
# The public mooncake wheels are built against CUDA 12 and import
# libcudart.so.12 even on a CUDA 13 image; the cu12 runtime they need is part
# of the bundle. Pinning that to LD_LIBRARY_PATH would only cover the processes
# the entrypoint starts, so put it in the loader configuration and let every
# child process inherit it.
CU12="${SITE}/nvidia/cuda_runtime/lib"
if [ -d "${CU12}" ]; then
  echo "${CU12}" > /etc/ld.so.conf.d/talea-cu12-runtime.conf
  echo "talea-runtime: cu12 runtime on the loader path: ${CU12}"
else
  echo "talea-runtime: no cu12 runtime in the bundle; mooncake imports will need libcudart.so.12 from elsewhere" >&2
fi

# --- toolkit layout -------------------------------------------------------
# The pip CUDA wheels lay out nvidia/cu13/lib, while the JIT compiler asks the
# linker for lib64 and for versionless .so names. When these links are missing
# the failure reads "cannot find -lcudart" rather than "wrong version", which
# costs an afternoon.
for candidate in "${SITE}/nvidia/cu13" "${SITE}/nvidia"; do
  [ -d "${candidate}/lib" ] || continue
  [ -e "${candidate}/lib64" ] || ln -s lib "${candidate}/lib64"
  for shared in "${candidate}"/lib/lib*.so.*; do
    [ -e "${shared}" ] || continue
    link="${shared%.so.*}.so"
    [ -e "${link}" ] || ln -s "$(basename "${shared}")" "${link}"
  done
done
ldconfig

# --- the source-identity checked Decode patch ------------------------------
# sglang_compat.py only rewrites the exact file it was written for and reports
# whether it did, so a version bump shows up as a no-op rather than a silent
# half-patch.
COMPAT="${ROOT}/onboarding/sglang_compat.py"
if [ -f "${COMPAT}" ]; then
  "${VENV}/bin/python" "${COMPAT}"
fi

# --- the toolkit must be usable, not merely present ------------------------
"${VENV}/bin/python" - <<'PY'
import sglang, torch
print("talea-runtime: sglang", sglang.__version__, "/ torch", torch.__version__)
PY

echo "talea-runtime: ready"
