#!/bin/bash
# Add one WireGuard peer to the management server and print its client config.
#
# usage: wg-add-peer.sh <label> [last-octet]
#
# The peer gets a single /32 inside 10.66.0.0/24. Its AllowedIPs is that same
# /24, never 0.0.0.0/0: a client on this tunnel keeps its own route to the
# internet, and a default route here would also drag its DNS through a
# management link that exists to carry one HTTP API.
#
# Three writes, all of them needed. The stanza file is what the config is built
# from, the rebuilt wg0.conf is what survives a reboot, and `wg syncconf` is
# what makes the peer live without dropping the interface.
set -eu

HERE=$(cd "$(dirname "$(readlink -f "$0")")" && pwd)
WG_DIR=${WG_DIR:-/etc/wireguard}
PEERS=$WG_DIR/peers
ENDPOINT=${WG_ENDPOINT:-36.138.20.89:51820}
LABEL=${1:?usage: wg-add-peer.sh <label> [last-octet]}
OCT=${2:-}

umask 077
mkdir -p "$PEERS/$LABEL"

if [ -z "$OCT" ]; then
  for o in $(seq 3 254); do
    if ! grep -qs "AllowedIPs = 10\.66\.0\.$o/32" "$PEERS"/*/peer.conf; then
      OCT=$o
      break
    fi
  done
fi
[ -n "${OCT:-}" ] || { echo "no free address in 10.66.0.0/24" >&2; exit 1; }
IP="10.66.0.$OCT"

if [ -s "$PEERS/$LABEL/private.key" ]; then
  echo "note: reusing the existing key for $LABEL"
else
  wg genkey > "$PEERS/$LABEL/private.key"
fi
PUB=$(wg pubkey < "$PEERS/$LABEL/private.key")

cat > "$PEERS/$LABEL/peer.conf" <<EOF
[Peer]
# $LABEL, added $(date -u +%FT%TZ)
PublicKey = $PUB
AllowedIPs = $IP/32
EOF

bash "$HERE/wg-build-conf.sh"

# syncconf applies the file as the whole peer set, which is exactly what the
# rebuild produced, and does it in place.
wg syncconf wg0 <(wg-quick strip wg0)

cat > "$PEERS/$LABEL/client.conf" <<EOF
[Interface]
PrivateKey = $(cat "$PEERS/$LABEL/private.key")
Address = $IP/32
MTU = 1420

[Peer]
PublicKey = $(cat "$WG_DIR/server.pub")
Endpoint = $ENDPOINT
AllowedIPs = 10.66.0.0/24
PersistentKeepalive = 25
EOF
chmod 600 "$PEERS/$LABEL/private.key" "$PEERS/$LABEL/client.conf"

echo "=== $LABEL -> $IP ==="
cat "$PEERS/$LABEL/client.conf"
