#!/bin/bash
# Rebuild wg0.conf from the interface header plus one file per peer.
#
# wg0.conf is generated, never edited. Every peer owns peers/<label>/peer.conf,
# so adding or removing one is a file operation and the file the daemon reads at
# boot cannot drift from the running interface.
set -eu

WG_DIR=${WG_DIR:-/etc/wireguard}
PEERS=$WG_DIR/peers

[ -s "$WG_DIR/header.conf" ] || { echo "missing $WG_DIR/header.conf" >&2; exit 1; }

umask 077
tmp=$(mktemp "$WG_DIR/.wg0.conf.XXXXXX")
cat "$WG_DIR/header.conf" > "$tmp"
if [ -d "$PEERS" ]; then
  for f in "$PEERS"/*/peer.conf; do
    [ -f "$f" ] || continue
    printf '\n' >> "$tmp"
    cat "$f" >> "$tmp"
  done
fi
mv "$tmp" "$WG_DIR/wg0.conf"
chmod 600 "$WG_DIR/wg0.conf"
