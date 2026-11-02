#!/bin/bash
# Sandbox test for the stage-B trust-domain scripts (plan P0-5, P1-12, P1-13).
#
# Builds a fake control-node filesystem and a fake NAS directory, then runs
# cci-pack-runtime.sh / cci-unpack-runtime.sh / cci-restore-plane.sh against
# them - positive paths and fault injections. No GPU, no sglang, no root.
# Run from anywhere:  bash deploy/alayanew/ops/test-stage-b-sandbox.sh
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
PACK=$HERE/cci-pack-runtime.sh
UNPACK=$HERE/cci-unpack-runtime.sh
RESTORE=$HERE/cci-restore-plane.sh

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "ok  - $1"; }
fail() { FAIL=$((FAIL+1)); echo "FAIL - $1"; }

expect_ok() { # desc, then command...
  local desc=$1; shift
  if "$@" >/dev/null 2>&1; then ok "$desc"; else fail "$desc"; fi
}
expect_fail() {
  local desc=$1; shift
  if "$@" >/dev/null 2>&1; then fail "$desc (unexpectedly succeeded)"; else ok "$desc"; fi
}

# Work under the repo, not /tmp: sandboxed environments and some containers
# restrict it, and the paths stay out of system temp. A fresh directory per
# run (pid-suffixed); artifacts are tiny, so no recursive delete is attempted
# here - recursive rm patterns trip sandbox/permission layers and can make
# nested scripts' writes vanish. Clean stale runs with:
#   rm -r deploy/alayanew/ops/.tmp-stage-b-test-*   (or from the repo root)
BASE=${TALEA_TEST_TMP_BASE:-.tmp-stage-b-test}
WORK=$PWD/$BASE-$$
mkdir -p "$WORK"
NODE=$WORK/node        # fake control-node /
NAS=$WORK/nas          # fake shared NAS runtime dir
OUT=$WORK/pack
mkdir -p "$NODE" "$NAS" "$OUT"

# ---- fake control node ------------------------------------------------
mkdir -p "$NODE/opt/tai-talea/bootstrap-src/tai_talea_bootstrap" \
         "$NODE/opt/tai-talea/logs" "$NODE/opt/tai-talea/run" \
         "$NODE/etc/tai-talea/onboarding" "$NODE/var/lib/tai-talea" \
         "$NODE/root" "$NODE/root/.cache/deep_gemm"
echo "pkg" > "$NODE/opt/tai-talea/bootstrap-src/tai_talea_bootstrap/__init__.py"
echo "log" > "$NODE/opt/tai-talea/logs/app.log"
echo "123" > "$NODE/opt/tai-talea/run/bootstrap.pid"
echo "config-with-ak-sk: top-secret" > "$NODE/etc/tai-talea/tai-talea.yaml"
echo "operator-token" > "$NODE/etc/tai-talea/bootstrap-token"
echo '{"os_version":"ubuntu-22.04","cuda":"13.0","python":"3.10","sglang":"0.5.22"}' > "$NODE/etc/tai-talea/profile.json"
echo "state" > "$NODE/etc/tai-talea/onboarding/state.json"
echo "sqlite" > "$NODE/var/lib/tai-talea/tai-talea.db"
printf '#!/bin/sh\necho tai-talea 0.9.8\n' > "$NODE/root/tai-talea"
printf '#!/bin/sh\necho gateway\n' > "$NODE/root/sgl-model-gateway"
echo "jit" > "$NODE/root/.cache/deep_gemm/x.cache"

# ---- pack -------------------------------------------------------------
echo "== pack =="
expect_ok "pack with passphrase" \
  env TALEA_PACK_ROOT=$NODE TALEA_PACK_OUT=$OUT TALEA_BACKUP_PASSPHRASE=test-pass \
  bash "$PACK"

BITS=$OUT/tai-talea-bits.tar
PRIV=$(ls "$OUT"/tai-talea-private-*.tar.gz.enc 2>/dev/null | head -1)
[ -f "$BITS" ] || { fail "bits tar was produced"; exit 1; }
[ -n "$PRIV" ] && [ -f "$PRIV" ] && ok "encrypted private archive was produced" || { fail "encrypted private archive was produced"; exit 1; }
[ -f "$OUT/SHA256SUMS" ] && [ -f "$OUT/tai-talea-manifest.sha256" ] && ok "SHA256SUMS + manifest produced" || fail "SHA256SUMS + manifest produced"

if tar -tf "$BITS" | grep -Eq '(tai-talea[^/]*\.yaml|token|onboarding|\.db|logs/|run/)'; then
  fail "worker bits tar carries no secrets/logs/run"
else
  ok "worker bits tar carries no secrets/logs/run"
fi
tar -tf "$BITS" | grep -q 'etc/tai-talea/profile.json' && ok "profile.json is in the worker domain" || fail "profile.json is in the worker domain"
tar -tf "$BITS" | grep -q 'opt/tai-talea/bootstrap-src/tai_talea_bootstrap/__init__.py' && ok "bootstrap source is in the worker domain" || fail "bootstrap source is in the worker domain"

expect_fail "pack without a passphrase refuses" \
  env TALEA_PACK_ROOT=$NODE TALEA_PACK_OUT=$WORK/nopass bash "$PACK"
[ ! -e "$WORK/nopass/tai-talea-bits.tar" ] && ok "refused pack left no bits tar behind" || fail "refused pack left no bits tar behind"

# ---- unpack: worker ----------------------------------------------------
echo "== unpack (worker) =="
# a fake venv tar and jit tar for the worker flow
mkdir -p "$WORK/venvsquash"
( cd "$WORK/venvsquash" && mkdir -p venv/bin venv/lib && echo fake > venv/bin/python && echo l > venv/lib/x.py && tar -cf "$OUT/venv-0.5.22-cu130.tar" venv )
echo jitcache > "$WORK/jitfile"
( cd "$WORK" && mkdir -p jitsrc/.cache && cp jitfile jitsrc/.cache/ && tar -cf "$NAS/jit-caches.tar" -C jitsrc .cache )
cp "$BITS" "$NAS/tai-talea-bits.tar"
cp "$OUT/venv-0.5.22-cu130.tar" "$NAS/"
cp "$OUT/SHA256SUMS" "$NAS/"
( cd "$NAS" && sha256sum jit-caches.tar >> SHA256SUMS )

FRESH=$WORK/fresh1
expect_ok "worker unpack (checksums present)" \
  env TALEA_ROOT=$FRESH TALEA_RUNTIME_DIR=$NAS TALEA_WORKER_ONLY=1 TALEA_SKIP_RUNTIME_VERIFY=1 \
  bash "$UNPACK"
[ -f "$FRESH/etc/tai-talea/profile.json" ] && ok "worker got profile.json" || fail "worker got profile.json"
[ ! -e "$FRESH/etc/tai-talea/tai-talea.yaml" ] && ok "worker has no control-plane yaml" || fail "worker has no control-plane yaml"
[ ! -e "$FRESH/etc/tai-talea/bootstrap-token" ] && ok "worker has no operator token" || fail "worker has no operator token"
[ ! -e "$FRESH/root/tai-talea" ] && ok "worker never restored the control-plane binary" || fail "worker never restored the control-plane binary"

# an OLD-layout bits tar that does carry secrets must be scrubbed, loudly
mkdir -p "$WORK/oldbits/etc/tai-talea"
cp "$NODE/etc/tai-talea/tai-talea.yaml" "$WORK/oldbits/etc/tai-talea/"
tar -cf "$WORK/old-bits.tar" -C "$WORK/oldbits" etc
mkdir -p "$WORK/nas-old"; cp "$NAS/venv-0.5.22-cu130.tar" "$NAS/jit-caches.tar" "$WORK/old-bits.tar" "$WORK/nas-old/"
mv "$WORK/nas-old/old-bits.tar" "$WORK/nas-old/tai-talea-bits.tar"
( cd "$NAS" && sha256sum venv-0.5.22-cu130.tar jit-caches.tar > SHA256SUMS )
( cd "$WORK/nas-old" && sha256sum venv-0.5.22-cu130.tar tai-talea-bits.tar jit-caches.tar > SHA256SUMS )
# restore the NAS to the full worker set for the later tests
( cd "$NAS" && sha256sum venv-0.5.22-cu130.tar tai-talea-bits.tar jit-caches.tar > SHA256SUMS )
FRESH2=$WORK/fresh2
expect_ok "worker unpack of the OLD secret-carrying bits tar" \
  env TALEA_ROOT=$FRESH2 TALEA_RUNTIME_DIR=$WORK/nas-old TALEA_WORKER_ONLY=1 TALEA_SKIP_RUNTIME_VERIFY=1 \
  bash "$UNPACK"
[ ! -e "$FRESH2/etc/tai-talea/tai-talea.yaml" ] && ok "old-layout yaml was scrubbed from the worker" || fail "old-layout yaml was scrubbed from the worker"

# ---- unpack: negative paths --------------------------------------------
echo "== unpack (negative) =="
mkdir -p "$WORK/nas-nosums"; cp "$NAS/venv-0.5.22-cu130.tar" "$NAS/jit-caches.tar" "$NAS/tai-talea-bits.tar" "$WORK/nas-nosums/"
expect_fail "missing SHA256SUMS refuses" \
  env TALEA_ROOT=$WORK/fresh3 TALEA_RUNTIME_DIR=$WORK/nas-nosums TALEA_WORKER_ONLY=1 TALEA_SKIP_RUNTIME_VERIFY=1 \
  bash "$UNPACK"
[ ! -e "$WORK/fresh3/opt/tai-talea/venv" ] && ok "refused unpack unpacked nothing" || fail "refused unpack unpacked nothing"

expect_ok "missing SHA256SUMS proceeds only with the loud override" \
  env TALEA_ROOT=$WORK/fresh4 TALEA_RUNTIME_DIR=$WORK/nas-nosums TALEA_WORKER_ONLY=1 TALEA_SKIP_RUNTIME_VERIFY=1 TALEA_ALLOW_MISSING_CHECKSUMS=1 \
  bash "$UNPACK"

mkdir -p "$WORK/nas-corrupt"; cp "$NAS/venv-0.5.22-cu130.tar" "$NAS/jit-caches.tar" "$NAS/tai-talea-bits.tar" "$NAS/SHA256SUMS" "$WORK/nas-corrupt/"
printf 'tampered' >> "$WORK/nas-corrupt/tai-talea-bits.tar"
expect_fail "checksum mismatch refuses" \
  env TALEA_ROOT=$WORK/fresh5 TALEA_RUNTIME_DIR=$WORK/nas-corrupt TALEA_WORKER_ONLY=1 TALEA_SKIP_RUNTIME_VERIFY=1 \
  bash "$UNPACK"

# non-worker unpack must NOT pick up a live snapshot that happens to sit there
mkdir -p "$WORK/livesrc/root"
cp "$NODE/root/tai-talea" "$WORK/livesrc/root/"
tar -czf "$NAS/live-2026-11-02.tar.gz" -C "$WORK/livesrc" root
FRESH6=$WORK/fresh6
expect_ok "control-node unpack (no live restore)" \
  env TALEA_ROOT=$FRESH6 TALEA_RUNTIME_DIR=$NAS TALEA_SKIP_RUNTIME_VERIFY=1 \
  bash "$UNPACK"
[ ! -e "$FRESH6/root/tai-talea" ] && ok "live snapshot is NOT auto-restored anymore" || fail "live snapshot is NOT auto-restored anymore"

# ---- restore-plane -------------------------------------------------------
echo "== restore plane =="
cp "$OUT/tai-talea-manifest.sha256" "$WORK/pinned.sha256"

expect_fail "restore without a pinned manifest refuses" \
  env TALEA_ROOT=$WORK/restore1 TALEA_MANIFEST=$WORK/nonexistent.sha256 TALEA_BACKUP_PASSPHRASE=test-pass \
  bash "$RESTORE" "$PRIV"

printf 'not an archive' > "$WORK/foreign.tar.gz"
expect_fail "restore with an archive the manifest does not pin refuses" \
  env TALEA_ROOT=$WORK/restore2 TALEA_MANIFEST=$WORK/pinned.sha256 TALEA_BACKUP_PASSPHRASE=test-pass \
  bash "$RESTORE" "$WORK/foreign.tar.gz"

TAMPER=$WORK/tampered.tar.gz.enc
cp "$PRIV" "$TAMPER"; printf 'x' >> "$TAMPER"
expect_fail "tampered archive fails the pinned hash" \
  env TALEA_ROOT=$WORK/restore3 TALEA_MANIFEST=$WORK/pinned.sha256 TALEA_BACKUP_PASSPHRASE=test-pass \
  bash "$RESTORE" "$TAMPER"

expect_fail "restore refuses to run on a worker" \
  env TALEA_WORKER_ONLY=1 TALEA_ROOT=$WORK/restore4 TALEA_MANIFEST=$WORK/pinned.sha256 TALEA_BACKUP_PASSPHRASE=test-pass \
  bash "$RESTORE" "$PRIV"

expect_fail "restore with the wrong passphrase refuses" \
  env TALEA_ROOT=$WORK/restore5 TALEA_MANIFEST=$WORK/pinned.sha256 TALEA_BACKUP_PASSPHRASE=wrong \
  bash "$RESTORE" "$PRIV"

expect_ok "restore with the pinned manifest + right passphrase" \
  env TALEA_ROOT=$WORK/restore6 TALEA_MANIFEST=$WORK/pinned.sha256 TALEA_BACKUP_PASSPHRASE=test-pass \
  bash "$RESTORE" "$PRIV"
[ -f "$WORK/restore6/etc/tai-talea/tai-talea.yaml" ] && ok "config restored to the control node" || fail "config restored to the control node"
[ -f "$WORK/restore6/var/lib/tai-talea/tai-talea.db" ] && ok "SQLite state restored" || fail "SQLite state restored"
[ -f "$WORK/restore6/etc/tai-talea/bootstrap-token" ] && ok "operator token restored" || fail "operator token restored"
[ ! -e "$WORK/restore6/opt/tai-talea/venv" ] && ok "restore does not touch the worker venv domain" || fail "restore does not touch the worker venv domain"

echo
echo "== stage B sandbox: $PASS passed, $FAIL failed =="
if [ "$FAIL" != 0 ]; then
  echo "artifacts kept for inspection: $WORK"
fi
[ "$FAIL" = 0 ]
