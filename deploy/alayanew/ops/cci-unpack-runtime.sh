#!/bin/bash
# Restore the baked GLM-5.3-Flash runtime into a fresh CCI container.
#
# A fresh container from alayanew-public/sglang:20250930 ships sglang 0.5.3rc0,
# which cannot load Glm5NextForConditionalGeneration. This script puts back the
# exact runtime proven on 2026-10-26: sglang 0.5.22 + torch 2.14.1+cu130 + the
# DeepGEMM/tilelang JIT caches.
#
# Idempotent: existing pieces are left alone, so it is safe to rerun after a
# partial failure. Takes ~2-3 minutes off the shared volume instead of ~25
# minutes of PyPI downloads.
set -u
SRC=${TALEA_RUNTIME_DIR:-/root/gongji/tai-talea-runtime}
echo "== restore start $(date -u +%FT%TZ) =="

for f in venv-0.5.22-cu130.tar tai-talea-bits.tar jit-caches.tar; do
  [ -f "$SRC/$f" ] || { echo "MISSING $SRC/$f"; exit 21; }
done

if [ -f "$SRC/SHA256SUMS" ]; then
  ( cd "$SRC" && sha256sum -c SHA256SUMS --quiet ) || { echo "CHECKSUM MISMATCH"; exit 22; }
  echo "checksums ok"
fi

mkdir -p /opt/tai-talea /etc/tai-talea /var/lib/tai-talea /opt/tai-talea/logs

# Presence is not enough: a container from the base image already has a venv
# (sglang 0.5.3rc0, no glm5_next), and reusing it would look like success while
# the launch still fails. Unpack only if the venv cannot import the model class
# this deployment exists for.
venv_ready() {
  [ -x /opt/tai-talea/venv/bin/python ] &&
    /opt/tai-talea/venv/bin/python -c \
      "from sglang.srt.models.glm5_next import Glm5NextForConditionalGeneration" 2>/dev/null
}
if venv_ready; then
  echo "venv already carries glm5_next"
else
  if [ -d /opt/tai-talea/venv ]; then
    echo "replacing venv without glm5_next (kept as venv.old)"
    rm -rf /opt/tai-talea/venv.old
    mv /opt/tai-talea/venv /opt/tai-talea/venv.old
  fi
  echo "unpacking venv"
  tar -C /opt/tai-talea -xf "$SRC/venv-0.5.22-cu130.tar"
fi

echo "unpacking runtime bits"
tar -C / -xf "$SRC/tai-talea-bits.tar"

echo "unpacking jit caches"
tar -C /root -xf "$SRC/jit-caches.tar"

# The live control plane and its configuration are not in the bits tarball
# (that one was baked on 2026-10-26, before 0.9.1-cci). If the operator left a
# snapshot next to the tars, prefer it - it is the exact build that was running.
if [ -f "$SRC/live-2026-11-02.tar.gz" ] && [ ! -x /root/tai-talea ]; then
  echo "unpacking the live snapshot (control plane + router + config)"
  tar -xzf "$SRC/live-2026-11-02.tar.gz" -C /
  chmod +x /root/tai-talea /root/sgl-model-gateway 2>/dev/null
fi

echo "== verify =="
/opt/tai-talea/venv/bin/python - <<'EOF'
import sglang, torch
from sglang.srt.models.glm5_next import Glm5NextForConditionalGeneration
print("sglang", sglang.__version__)
print("torch", torch.__version__, "cuda", torch.version.cuda, "avail", torch.cuda.is_available())
print("glm5 OK", Glm5NextForConditionalGeneration.__name__)
EOF
echo "== restore done $(date -u +%FT%TZ) =="
