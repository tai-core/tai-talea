#!/bin/bash
# Restore the baked GLM-5.3-Flash runtime into a fresh CCI container.
#
# A fresh container from alayanew-public/sglang:20250930 ships sglang 0.5.3rc0,
# which cannot load Glm5NextForConditionalGeneration. This script puts back the
# exact runtime proven on 2026-10-26: sglang 0.5.22 + torch 2.14.1+cu130 + the
# DeepGEMM/tilelang JIT caches.
#
# Trust rules (plan P1-12, P0-5):
#   - SHA256SUMS is mandatory. A volume without checksums is not a shortcut,
#     it is an unsigned artifact set; refusal is the default.
#   - This script never restores the control plane. Live snapshots and the
#     private archive are restored on the control node only, explicitly, with
#     a pinned-hash check: ops/cci-restore-plane.sh.
#   - Worker mode (TALEA_WORKER_ONLY=1) additionally scrubs any control-plane
#     config that came out of the OLD tai-talea-bits.tar, which was baked
#     before the secret split and carries the tenant AK/SK.
#
# Idempotent: existing pieces are left alone, so it is safe to rerun after a
# partial failure. Takes ~2-3 minutes off the shared volume instead of ~25
# minutes of PyPI downloads.
#
# Environment:
#   TALEA_RUNTIME_DIR                 source dir (default /root/gongji/tai-talea-runtime)
#   TALEA_ROOT                        fake / for sandbox runs
#   TALEA_WORKER_ONLY                 1 = worker: scrub control-plane secrets after unpack
#   TALEA_ALLOW_MISSING_CHECKSUMS     1 = escape hatch for volumes without SHA256SUMS (loud)
set -u
SRC=${TALEA_RUNTIME_DIR:-/root/gongji/tai-talea-runtime}
ROOT=${TALEA_ROOT:-}
ROOT=${ROOT%/}
echo "== restore start $(date -u +%FT%TZ) =="

die() { printf '!! %s\n' "$*" >&2; exit 1; }

for f in venv-0.5.22-cu130.tar tai-talea-bits.tar jit-caches.tar; do
  [ -f "$SRC/$f" ] || die "MISSING $SRC/$f"
done

# Checksums are the only integrity check a worker has, and the NAS checksum
# file shares a trust domain with the tars - which is exactly why the control
# plane pins its own manifest (cci-restore-plane.sh). For the worker domain
# this is still worth enforcing: a truncated copy (the real failure seen on
# 12 GB CephFS) is caught here.
if [ -f "$SRC/SHA256SUMS" ]; then
  ( cd "$SRC" && sha256sum -c SHA256SUMS --quiet ) || die "CHECKSUM MISMATCH"
  echo "checksums ok"
elif [ "${TALEA_ALLOW_MISSING_CHECKSUMS:-0}" = "1" ]; then
  echo "!! TALEA_ALLOW_MISSING_CHECKSUMS=1: restoring WITHOUT integrity checks." >&2
  echo "!! Re-hash the volume (sha256sum venv-*.tar tai-talea-bits.tar jit-caches*.tar > SHA256SUMS)" >&2
  echo "!! and rerun without the override. This run must not become the norm." >&2
else
  die "no SHA256SUMS in $SRC - integrity cannot be verified. Hash the volume and rerun (or set TALEA_ALLOW_MISSING_CHECKSUMS=1 once, loudly, if you must)"
fi

mkdir -p "$ROOT/opt/tai-talea" "$ROOT/etc/tai-talea" "$ROOT/var/lib/tai-talea" "$ROOT/opt/tai-talea/logs" "$ROOT/root"

# Presence is not enough: a container from the base image already has a venv
# (sglang 0.5.3rc0, no glm5_next), and reusing it would look like success while
# the launch still fails. Unpack only if the venv cannot import the model class
# this deployment exists for.
venv_ready() {
  [ -x "$ROOT/opt/tai-talea/venv/bin/python" ] &&
    "$ROOT/opt/tai-talea/venv/bin/python" -c \
      "from sglang.srt.models.glm5_next import Glm5NextForConditionalGeneration" 2>/dev/null
}
if venv_ready; then
  echo "venv already carries glm5_next"
else
  if [ -d "$ROOT/opt/tai-talea/venv" ]; then
    echo "replacing venv without glm5_next (kept as venv.old)"
    rm -rf "$ROOT/opt/tai-talea/venv.old"
    mv "$ROOT/opt/tai-talea/venv" "$ROOT/opt/tai-talea/venv.old"
  fi
  echo "unpacking venv"
  tar -C "$ROOT/opt/tai-talea" -xf "$SRC/venv-0.5.22-cu130.tar" || die "venv unpack failed"
fi

echo "unpacking runtime bits"
# TALEA_ROOT is normally unset: the empty string must become "/", not an
# argument-less -C (which fails as "tar: : Cannot open").
tar -C "${ROOT:-/}" -xf "$SRC/tai-talea-bits.tar" || die "bits unpack failed"

echo "unpacking jit caches"
tar -C "$ROOT/root" -xf "$SRC/jit-caches.tar" || die "jit caches unpack failed"

# Worker scrub for the OLD bits tar: it was baked on 2026-10-26 with
# /etc/tai-talea inside, tenant AK/SK included. New packs (cci-pack-runtime.sh)
# whitelist only profile.json, so nothing needs scrubbing - but the old tar is
# what is on the NAS today, and a worker that restored its config would hold
# tenant credentials it never needs.
if [ "${TALEA_WORKER_ONLY:-0}" = "1" ]; then
  found=$(ls "$ROOT"/etc/tai-talea/tai-talea*.yaml 2>/dev/null || true)
  if [ -n "$found" ]; then
    echo "!! worker-only: the bits tar carried control-plane config (the OLD layout did exactly this):"
    echo "$found"
    rm -f "$ROOT"/etc/tai-talea/tai-talea*.yaml
    echo "!! worker-only: removed. That file carried AK/SK - rotate credentials that entered the old tar (plan P0-5)."
  fi
  [ -e "$ROOT/root/tai-talea" ] &&
    echo "!! worker-only: /root/tai-talea exists - a live snapshot or old bits landed here; workers do not run the control plane" >&2
  echo "worker-only: control-plane secrets scrubbed"
else
  # No auto-restore of live snapshots, ever: the control plane comes back only
  # through the pinned-hash path (ops/cci-restore-plane.sh). Restore order is
  # documented in deploy/alayanew/README.md.
  echo "control plane not touched: restore it explicitly with ops/cci-restore-plane.sh (pinned-hash path)"
fi

# The runtime verify needs a real GPU-capable venv; sandbox runs set
# TALEA_SKIP_RUNTIME_VERIFY=1 to test the plumbing without sglang.
if [ "${TALEA_SKIP_RUNTIME_VERIFY:-0}" != "1" ]; then
  echo "== verify =="
  "$ROOT/opt/tai-talea/venv/bin/python" - <<'EOF'
import sglang, torch
from sglang.srt.models.glm5_next import Glm5NextForConditionalGeneration
print("sglang", sglang.__version__)
print("torch", torch.__version__, "cuda", torch.version.cuda, "avail", torch.cuda.is_available())
print("glm5 OK", Glm5NextForConditionalGeneration.__name__)
EOF
else
  echo "== verify skipped (TALEA_SKIP_RUNTIME_VERIFY=1) =="
fi

echo "== restore done $(date -u +%FT%TZ) =="
