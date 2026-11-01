#!/bin/bash
# Remove one WireGuard peer.
#
# usage: wg-del-peer.sh <label>
#
# Deleting the stanza file and rebuilding is enough for the reboot case;
# `wg syncconf` is what takes the peer out of the running interface, and it does
# that for every peer the new file no longer lists.
set -eu

HERE=$(cd "$(dirname "$(readlink -f "$0")")" && pwd)
WG_DIR=${WG_DIR:-/etc/wireguard}
PEERS=$WG_DIR/peers
LABEL=${1:?usage: wg-del-peer.sh <label>}

[ -d "$PEERS/$LABEL" ] || { echo "no such peer: $LABEL" >&2; exit 1; }

if [ -s "$PEERS/$LABEL/private.key" ]; then
  PUB=$(wg pubkey < "$PEERS/$LABEL/private.key")
  wg set wg0 peer "$PUB" remove 2>/dev/null || true
fi

rm -rf "${PEERS:?}/$LABEL"
bash "$HERE/wg-build-conf.sh"
wg syncconf wg0 <(wg-quick strip wg0)

echo "removed $LABEL"
wg show wg0 peers | while read -r p; do echo "  remaining: $p"; done
