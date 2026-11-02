#!/bin/bash
# Restore the control plane from a private archive produced by
# ops/cci-pack-runtime.sh (plan P1-12, P1-13).
#
# This is the ONLY sanctioned path back to a running control plane build.
# cci-unpack-runtime.sh no longer auto-restores live snapshots: picking up
# whatever tar happens to sit next to the runtime tars is how stale binaries
# and leaked credentials both come back.
#
# Trust model (P1-13): the NAS SHA256SUMS shares a trust domain with the tars,
# so it cannot vouch for the private archive. This script verifies the archive
# against a manifest stored OUTSIDE the NAS (default
# /etc/tai-talea/artifact-manifest.sha256, as written by cci-pack-runtime.sh).
# A tampered tar plus a tampered NAS checksum list does not pass; a missing
# manifest is a refusal, not a warning.
#
# Usage (on the control node, never a worker):
#   TALEA_BACKUP_PASSPHRASE=... ops/cci-restore-plane.sh /path/tai-talea-private-20261102.tar.gz.enc
#
# Environment:
#   TALEA_MANIFEST           pinned manifest (default /etc/tai-talea/artifact-manifest.sha256)
#   TALEA_ROOT               fake / for sandbox runs
#   TALEA_BACKUP_PASSPHRASE  needed for .enc archives
set -eu

ARCHIVE=${1:?usage: cci-restore-plane.sh <tai-talea-private-*.tar.gz[.enc]>}
R=${TALEA_ROOT:-}
R=${R%/}
MANIFEST=${TALEA_MANIFEST:-/etc/tai-talea/artifact-manifest.sha256}

die() { printf '!! %s\n' "$*" >&2; exit 1; }

[ -f "$ARCHIVE" ] || die "archive $ARCHIVE not found"
case "$ARCHIVE" in
  *.tar.gz.enc) ENCRYPTED=1 ;;
  *.tar.gz)     ENCRYPTED=0 ;;
  *) die "refusing an unrecognized archive type: $ARCHIVE (expect tai-talea-private-*.tar.gz[.enc])" ;;
esac

# A worker must never restore the control plane; the flag is how cci-unpack-
# runtime.sh marks one, and this script refuses to run alongside it.
[ "${TALEA_WORKER_ONLY:-0}" = "1" ] && die "TALEA_WORKER_ONLY=1: a worker never restores the control plane"

# ------------------------------------------------ pinned manifest verification
[ -f "$MANIFEST" ] || die "pinned manifest $MANIFEST not found - restore refuses to proceed without a trust anchor outside the NAS (plan P1-13); pack with ops/cci-pack-runtime.sh and store the manifest off the shared volume"

NAME=$(basename "$ARCHIVE")
# The checksum line may use binary mode ("hash *name", as some sha256sum
# builds emit) or text mode ("hash  name"); accept both.
PINNED_LINE=$(grep -E "^[0-9a-f]{64} [ *]${NAME}\$" "$MANIFEST" || true)
[ -n "$PINNED_LINE" ] || die "$NAME is not pinned in $MANIFEST - an archive the manifest does not know is either foreign or newer than the manifest; re-run cci-pack-runtime.sh and re-pin before restoring"

echo "$PINNED_LINE" > "$(dirname "$ARCHIVE")/.pinned-check"
( cd "$(dirname "$ARCHIVE")" && sha256sum -c .pinned-check --quiet ) ||
  die "$NAME does not match the pinned hash - the archive or the NAS copy has changed; do not restore it"
rm -f "$(dirname "$ARCHIVE")/.pinned-check"
echo "pinned hash ok: $NAME"

# ------------------------------------------------------------- decrypt + unpack
# The decrypted copy lands next to the archive, not in /tmp: the archive dir is
# where we already wrote .pinned-check, it is the trusted side of the machine,
# and some sandboxed environments mangle /tmp paths badly enough that tar sees
# a "C:" drive prefix and reads it as a remote host spec.
WORKDIR=$(dirname "$ARCHIVE")
PLAIN=$WORKDIR/.restore-$.tar.gz
trap 'rm -f "$PLAIN"' EXIT

if [ "$ENCRYPTED" = "1" ]; then
  : "${TALEA_BACKUP_PASSPHRASE:?set TALEA_BACKUP_PASSPHRASE to decrypt $NAME}"
  command -v openssl >/dev/null 2>&1 || die "openssl not found - it decrypts the private archive"
  openssl enc -d -aes-256-cbc -pbkdf2 -in "$ARCHIVE" -out "$PLAIN" -pass env:TALEA_BACKUP_PASSPHRASE ||
    die "decryption failed - wrong passphrase or a corrupt archive"
else
  echo "!! restoring an UNENCRYPTED private archive - treat every credential inside as exposed; repack encrypted" >&2
  cp "$ARCHIVE" "$PLAIN"
fi

echo "unpacking into ${R:-/}"
mkdir -p "${R:-/}"
tar -C "${R:-/}" -xzf "$PLAIN"

for b in root/tai-talea root/sgl-model-gateway; do
  [ -e "${R:-/}/$b" ] && chmod +x "${R:-/}/$b"
done

echo "== verify =="
if [ -x "${R:-/}/root/tai-talea" ] || [ -x /root/tai-talea ]; then
  "${R:-/}/root/tai-talea" -version 2>/dev/null || /root/tai-talea -version 2>/dev/null || echo "(binary version print skipped)"
fi
[ -f "${R:-/}/etc/tai-talea/tai-talea.yaml" ] && echo "config present: etc/tai-talea/tai-talea.yaml"
[ -f "${R:-/}/var/lib/tai-talea/tai-talea.db" ] && echo "state present: var/lib/tai-talea/tai-talea.db"

echo "ok: control plane restored from $NAME (pinned hash verified)"
echo "next: start the plane and the Router with ops/cci-start-plane.sh, then the edge (edge/cci-edge.sh)"
