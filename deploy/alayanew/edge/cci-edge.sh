#!/bin/bash
# The public door for a CCI deployment: nginx (API key) + optional WireGuard.
#
# Same shape as the China Mobile cluster (deploy/kunhaoyuan/) *minus the tunnel*:
# CCI only maps TCP http/https ports, so a remote peer has no address to reach
# UDP 51820 on. wg0 is still wired up here because it costs nothing and the day
# the platform publishes UDP this is the only missing piece; until then treat it
# as a local-only interface. See ../README.md §3.3.
#
#   probe   report what this container can actually do - run this first
#   up      render the nginx config, start the door, and bring up wg0 if possible
#   down    stop the door (leaves wg0 alone unless --with-wg)
#   status  what is listening, and is the key enforced
#
# The key comes from TALEA_API_KEY. Changing it means re-running `up`, which is
# why the generated conf is never edited by hand.
set -u

EDGE_DIR=$(cd "$(dirname "$0")" && pwd)
CONF=/etc/nginx/conf.d/talea.conf
LOG_DIR=/opt/tai-talea/logs
WG_DIR=/etc/wireguard
WG_PORT=${TALEA_WG_PORT:-51820}
WG_NET=${TALEA_WG_NET:-10.66.0.1/24}

say() { printf '%s\n' "$*"; }

need_key() {
  : "${TALEA_API_KEY:?set TALEA_API_KEY (e.g. openssl rand -hex 16) - the door has no other authentication}"
  # The key is spliced into a sed replacement and an nginx map pattern. `|`, `&`,
  # `\` and whitespace all mean something in one of the two, and a key that
  # renders into a broken map fails at `nginx -t` or, worse, matches nothing.
  case "$TALEA_API_KEY" in
    *[!A-Za-z0-9_-]*)
      say "!! TALEA_API_KEY may only contain A-Z a-z 0-9 _ - (got ${#TALEA_API_KEY} chars)"
      return 1 ;;
  esac
}

# --------------------------------------------------------------------- probe
probe() {
  say "== what this container can do =="
  if [ -c /dev/net/tun ]; then
    say "tun device        yes (/dev/net/tun)"
  else
    say "tun device        NO  -> WireGuard cannot run here"
  fi
  if command -v wg >/dev/null 2>&1; then
    say "wg tool           yes ($(wg --version 2>/dev/null | head -1))"
  else
    say "wg tool           no  (apt-get install -y wireguard-tools, needs the tun device anyway)"
  fi
  if command -v nginx >/dev/null 2>&1; then
    say "nginx             yes ($(nginx -v 2>&1 | head -1))"
  else
    say "nginx             no  -> 'up' will try apt-get install -y nginx"
  fi
  # CAP_NET_ADMIN check that leaves nothing behind. The result feeds the
  # verdict below - a container without it cannot create wg0, and a verdict
  # that ignored this check would point at a tunnel that can never come up.
  cap="no"
  if command -v ip >/dev/null 2>&1; then
    if ip link add talea-cap-test type dummy 2>/dev/null; then
      ip link del talea-cap-test 2>/dev/null
      cap="yes"
      say "CAP_NET_ADMIN     yes"
    else
      say "CAP_NET_ADMIN     NO  -> cannot create interfaces (WireGuard out, nginx still fine)"
    fi
  else
    say "ip tool           no  -> cannot test CAP_NET_ADMIN"
  fi
  say ""
  say "verdict: $(
    if [ -c /dev/net/tun ] && [ "$cap" = "yes" ] && command -v wg >/dev/null 2>&1; then
      echo "wg0 can be brought up, but CCI publishes TCP only - no peer can reach it; the door still faces the internet"
    else
      echo "no tunnel (and CCI maps no UDP anyway) - the door listens on 0.0.0.0:8000 and the API key is the only defence"
    fi
  )"
}

# ------------------------------------------------------------------------ up
render_conf() {
  need_key || return 1
  install -d -m 755 "$LOG_DIR"
  # nginx may not be installed yet (start_nginx installs it); the candidate
  # still has to be writable now so the render happens before anything touches
  # the live config.
  mkdir -p "$(dirname "$CONF")"
  # Render to a sibling candidate first: conf.d/*.conf does not match
  # talea.conf.new, so the candidate is inert until it is moved into place.
  # Nothing overwrites the live config before it has passed nginx -t.
  sed "s|__API_KEY__|${TALEA_API_KEY}|g" "$EDGE_DIR/nginx-talea.conf.tmpl" > "$CONF.new" \
    || { rm -f "$CONF.new"; say "!! failed to render the candidate"; return 1; }
  chmod 600 "$CONF.new" 2>/dev/null || true
  say "rendered candidate $CONF.new"
}

start_nginx() {
  if ! command -v nginx >/dev/null 2>&1; then
    say "installing nginx"
    apt-get update -qq && apt-get install -y -qq nginx >/dev/null || {
      say "!! apt-get failed - upload a static nginx or skip the door entirely"
      return 1
    }
  fi
  # The distribution's default site also listens on 80; harmless, but it is an
  # unauthenticated page, so drop it.
  rm -f /etc/nginx/sites-enabled/default
  # Step 1: the config as it stands must be valid - if it is not, the problem
  # predates this run and swapping files would only confuse the post-mortem.
  if ! nginx -t >/dev/null 2>&1; then
    { nginx -t; } 2>&1 | tail -3 || true
    say "!! the current nginx config is invalid; refusing to touch it"
    return 1
  fi
  # Step 2: swap in the candidate, then test the result as the live config.
  # `nginx -t 2>&1 | tail` would report tail's exit status and mask the failure,
  # which is how a broken key render could look like a deployed door.
  local backup=""
  if [ -f "$CONF" ]; then
    cp "$CONF" "$CONF.bak" && backup="$CONF.bak"
  fi
  mv "$CONF.new" "$CONF" || return 1
  if ! nginx -t >/dev/null 2>&1; then
    say "!! the rendered config failed nginx -t; restoring the previous one"
    { nginx -t; } 2>&1 | tail -3 || true
    if [ -n "$backup" ]; then mv -f "$backup" "$CONF"; else rm -f "$CONF"; fi
    return 1
  fi
  [ -n "$backup" ] && rm -f "$backup"
  # Step 3: apply, then prove the listener exists before claiming success.
  if pgrep -x nginx >/dev/null; then
    nginx -s reload || { say "!! nginx -s reload failed"; return 1; }
    say "nginx reloaded"
  else
    nginx || { say "!! nginx failed to start"; return 1; }
    say "nginx started"
  fi
  local ok=""
  for _ in $(seq 1 10); do
    ss -lnt 2>/dev/null | grep -q ':8000 ' && { ok=1; break; }
    sleep 1
  done
  [ -n "$ok" ] || { say "!! nothing is listening on 8000 after the reload"; return 1; }
  say "ok: nginx is listening on :8000"
}

start_wg() {
  if [ ! -c /dev/net/tun ] || ! command -v wg >/dev/null 2>&1; then
    say "wg: skipped (no tun device or no wg tool) - door is on 0.0.0.0:8000"
    return 0
  fi
  install -d -m 700 "$WG_DIR"
  if [ ! -f "$WG_DIR/server.key" ]; then
    umask 077
    wg genkey > "$WG_DIR/server.key"
    wg pubkey < "$WG_DIR/server.key" > "$WG_DIR/server.pub"
    say "wg: generated server keypair"
  fi
  if [ ! -f "$WG_DIR/wg0.conf" ]; then
    # No NAT, no forwarding: a peer that reaches 10.66.0.1 stops at this
    # container's nginx. Peers are added by wg-add-peer.sh (deploy/kunhaoyuan/ops),
    # which rebuilds wg0.conf from header.conf + peers/<label>/peer.conf.
    cat > "$WG_DIR/header.conf" <<EOF
[Interface]
Address = ${WG_NET}
ListenPort = ${WG_PORT}
PrivateKey = $(cat "$WG_DIR/server.key")
EOF
    cp "$WG_DIR/header.conf" "$WG_DIR/wg0.conf"
    say "wg: wrote a header-only wg0.conf (add peers before expecting a client to connect)"
  fi
  if wg show wg0 >/dev/null 2>&1; then
    wg syncconf wg0 <(wg-quick strip wg0) 2>/dev/null && say "wg: wg0 reconfigured"
  else
    wg-quick up wg0 2>&1 | tail -3 || { say "wg: could not bring up wg0"; return 0; }
  fi
  say "wg: $(wg show wg0 | head -2 | tr '\n' ' ')"
  say "wg: public key = $(cat "$WG_DIR/server.pub")"
  say "wg: NOTE no UDP port is published by CCI - this interface is local-only for now"
}

up() {
  render_conf || return 1
  start_nginx || return 1
  start_wg
  say ""
  say "== door summary =="
  say "listening: $(ss -lntp 2>/dev/null | awk '/:8000 /{print $4}' | head -1)"
  say "key set  : ${TALEA_API_KEY:0:4}… (${#TALEA_API_KEY} chars)"
  say "check    : curl -s -o /dev/null -w '%{http_code}\\n' localhost:8000/health   # 200"
  say "           curl -s -o /dev/null -w '%{http_code}\\n' localhost:8000/v1/models # 401 without key"
}

# ---------------------------------------------------------------------- down
down() {
  if pgrep -x nginx >/dev/null; then
    nginx -s stop 2>/dev/null && say "nginx stopped"
  else
    say "nginx not running"
  fi
  if [ "${1:-}" = "--with-wg" ] && wg show wg0 >/dev/null 2>&1; then
    wg-quick down wg0 2>/dev/null && say "wg0 down"
  fi
}

# -------------------------------------------------------------------- status
status() {
  say "== listeners =="
  ss -lntp 2>/dev/null | awk 'NR==1 || /:(8000|8001|8787|9001|9002|9101|9102) /'
  say ""
  say "== 9001/9002 must have NO listener (the platform publishes them anyway) =="
  if ss -lnt 2>/dev/null | grep -qE ':(9001|9002) '; then
    say "!! something is listening on 9001/9002 - the internet can reach it"
  else
    say "ok: 9001/9002 are unbound"
  fi
  say ""
  say "== key enforcement =="
  say "no key : $(curl -s -m 5 -o /dev/null -w '%{http_code}' localhost:8000/v1/models 2>/dev/null || echo down)"
  if [ -n "${TALEA_API_KEY:-}" ]; then
    say "with key: $(curl -s -m 5 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TALEA_API_KEY" localhost:8000/v1/models 2>/dev/null || echo down)"
  fi
  say "health : $(curl -s -m 5 -o /dev/null -w '%{http_code}' localhost:8000/health 2>/dev/null || echo down)"
}

case "${1:-}" in
  probe)  probe ;;
  up)     up ;;
  down)   down "${2:-}" ;;
  status) status ;;
  *)      sed -n '2,18p' "$0"; exit 2 ;;
esac
