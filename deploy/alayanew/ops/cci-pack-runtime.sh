#!/bin/bash
# Pack the CCI runtime into two trust domains (plan P0-5, P1-13).
#
#   worker domain    no secrets, lives on the shared NAS:
#                      tai-talea-bits.tar        /opt/tai-talea (no venv/logs/run)
#                                                + etc/tai-talea/profile.json ONLY
#                      SHA256SUMS                hashes of the worker tars
#   private domain   control plane only, encrypted:
#                      tai-talea-private-<date>.tar.gz.enc
#                                                /root binaries, /etc/tai-talea
#                                                (yaml, tokens, onboarding),
#                                                /var/lib/tai-talea (SQLite)
#   manifest         tai-talea-manifest.sha256   pins EVERY hash above.
#
# The manifest is the trust anchor (plan P1-13): the NAS SHA256SUMS shares a
# trust domain with the tars it hashes, so a restore of the control plane
# (ops/cci-restore-plane.sh) verifies against this manifest, which must be
# stored OUTSIDE the NAS - default /etc/tai-talea/artifact-manifest.sha256 on
# the control node. Tampering with both a tar and the NAS checksums must not
# pass a restore.
#
# The old layout is the leak this script exists to stop: tai-talea-bits.tar
# carried /etc/tai-talea with the tenant AK/SK, and live snapshots sat in the
# same shared directory. Every credential that entered the old layout must be
# rotated (plan P0-5 item 4) - packing a clean tar does not un-leak the old one.
#
# Environment:
#   TALEA_PACK_ROOT          fake / for sandbox runs (default: real /)
#   TALEA_PACK_OUT           output directory (default: $PWD)
#   TALEA_RUNTIME_DIR        existing NAS dir with venv/jit tars; their hashes
#                            are folded into SHA256SUMS and the manifest
#   TALEA_BACKUP_PASSPHRASE  required - encrypts the private archive
#   TALEA_PRIVATE_PLAIN      1 = emit the private archive unencrypted (escape
#                            hatch; loud warning, still a last resort)
set -eu

R=${TALEA_PACK_ROOT:-}
R=${R%/}
OUT=${TALEA_PACK_OUT:-$PWD}
STAMP=$(date -u +%Y%m%d)
BITS_TAR=tai-talea-bits.tar
PRIVATE_TAR=tai-talea-private-$STAMP.tar.gz

mkdir -p "$OUT"

die() { printf '!! %s\n' "$*" >&2; exit 1; }

# ------------------------------------------------------------- preflight
[ -d "$R/opt/tai-talea" ] || die "$R/opt/tai-talea not found - run this on a node that has the runtime unpacked"
[ -f "$R/etc/tai-talea/profile.json" ] || die "$R/etc/tai-talea/profile.json not found - the worker bootstrap needs it"

if [ "${TALEA_PRIVATE_PLAIN:-0}" != "1" ]; then
  : "${TALEA_BACKUP_PASSPHRASE:?set TALEA_BACKUP_PASSPHRASE to encrypt the private archive}"
  command -v openssl >/dev/null 2>&1 || die "openssl not found - it encrypts the private archive"
fi
command -v sha256sum >/dev/null 2>&1 || die "sha256sum not found"

# ------------------------------------------------- worker bits (whitelist)
# Whitelist, not blacklist: the worker domain gets exactly opt/tai-talea
# (minus the things that are not bits) plus the one file from etc/tai-talea
# the bootstrap actually reads. Anything else - yaml, tokens, onboarding,
# SQLite - stays out by construction, so a future file dropped into
# /etc/tai-talea cannot ride along.
echo "== packing worker bits =="
tar -C "$R" -cf "$OUT/$BITS_TAR" \
  --exclude='opt/tai-talea/venv' \
  --exclude='opt/tai-talea/venv.old' \
  --exclude='opt/tai-talea/logs' \
  --exclude='opt/tai-talea/run' \
  opt/tai-talea etc/tai-talea/profile.json

# Refuse to ship a worker tar with secrets in it. The whitelist should make
# this unreachable; the scan is what turns "should" into "verified" every run.
if tar -tf "$OUT/$BITS_TAR" | grep -E '(^|/)(tai-talea[^/]*\.yaml|[^/]*token[^/]*|onboarding(/|$)|[^/]*\.(db|sqlite))' >/dev/null; then
  tar -tf "$OUT/$BITS_TAR" | grep -E '(^|/)(tai-talea[^/]*\.yaml|[^/]*token[^/]*|onboarding(/|$)|[^/]*\.(db|sqlite))'
  die "the worker bits tar contains secret-looking members (listed above) - fix the whitelist, never ship this"
fi
echo "bits member scan: no secrets"

# ------------------------------------------------------ private (encrypted)
echo "== packing the private domain =="
PRIVATE_MEMBERS=""
for m in root/tai-talea root/sgl-model-gateway etc/tai-talea var/lib/tai-talea; do
  [ -e "$R/$m" ] && PRIVATE_MEMBERS="$PRIVATE_MEMBERS $m"
done
[ -n "$PRIVATE_MEMBERS" ] || die "nothing to pack: none of root/tai-talea, root/sgl-model-gateway, etc/tai-talea, var/lib/tai-talea exist under $R"
# shellcheck disable=SC2086
tar -C "$R" -czf "$OUT/$PRIVATE_TAR" $PRIVATE_MEMBERS

PRIVATE_OUT=$OUT/$PRIVATE_TAR.enc
if [ "${TALEA_PRIVATE_PLAIN:-0}" = "1" ]; then
  echo "!! TALEA_PRIVATE_PLAIN=1: the private archive is written UNENCRYPTED." >&2
  echo "!! It carries the tenant AK/SK and every token. Move it off the shared" >&2
  echo "!! NAS immediately and treat every credential inside as exposed." >&2
  mv "$OUT/$PRIVATE_TAR" "$PRIVATE_OUT"
else
  openssl enc -aes-256-cbc -pbkdf2 -salt -in "$OUT/$PRIVATE_TAR" \
    -out "$PRIVATE_OUT" -pass env:TALEA_BACKUP_PASSPHRASE
  rm -f "$OUT/$PRIVATE_TAR"
fi

# ------------------------------------------------- worker SHA256SUMS (NAS)
echo "== hashing =="
SUMS=$OUT/SHA256SUMS
: > "$SUMS"
for f in "$BITS_TAR"; do
  ( cd "$OUT" && sha256sum "$f" ) >> "$SUMS"
done
if [ -n "${TALEA_RUNTIME_DIR:-}" ]; then
  for f in venv-*.tar jit-caches*.tar; do
    if [ -f "$TALEA_RUNTIME_DIR/$f" ]; then
      ( cd "$TALEA_RUNTIME_DIR" && sha256sum "$f" ) >> "$SUMS"
    else
      echo "note: $TALEA_RUNTIME_DIR/$f not found, not hashed" >&2
    fi
  done
else
  echo "note: TALEA_RUNTIME_DIR not set - venv/jit tars are NOT in SHA256SUMS; run this again on the control node with it set" >&2
fi
echo "worker SHA256SUMS:"
cat "$SUMS"

# ------------------------------------------------ manifest (trust anchor)
# Everything, including the private archive and SHA256SUMS itself. This file
# belongs OFF the shared NAS: copy it to /etc/tai-talea/artifact-manifest.sha256
# (the restore script's default) or the operator's own store.
MANIFEST=$OUT/tai-talea-manifest.sha256
: > "$MANIFEST"
{
  echo "# tai-talea artifact manifest - pin these hashes OUTSIDE the NAS."
  echo "# Verify a restore against this file: TALEA_MANIFEST=<path> ops/cci-restore-plane.sh <archive>"
  echo "# Packed $(date -u +%FT%TZ) on ${TALEA_PACK_ROOT:-/}"
} > "$MANIFEST"
( cd "$OUT" && sha256sum "$BITS_TAR" "$PRIVATE_TAR.enc" SHA256SUMS ) >> "$MANIFEST"
if [ -n "${TALEA_RUNTIME_DIR:-}" ]; then
  ( cd "$TALEA_RUNTIME_DIR" && sha256sum venv-*.tar jit-caches*.tar 2>/dev/null ) >> "$MANIFEST" || true
fi

echo "== done =="
cat "$MANIFEST"
echo
echo "next:"
echo "  1. copy $MANIFEST to /etc/tai-talea/artifact-manifest.sha256 (control node, NOT the NAS)"
echo "  2. ship $BITS_TAR + SHA256SUMS to the worker domain of the NAS"
echo "  3. keep $PRIVATE_OUT in the control-plane domain (encrypted)"
echo "  4. rotate every credential that ever entered the OLD tai-talea-bits.tar or live-* snapshots (plan P0-5)"
