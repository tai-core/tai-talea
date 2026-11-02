#!/bin/bash
# Start the Router and the control plane on the first CCI container, then
# report what the control plane sees after its first reconcile passes.
#
# Preconditions (run these first, in this order):
#   1. ops/cci-unpack-runtime.sh  - the runtime exists on this container
#   2. ops/cci-start-worker.sh    - THIS container's bootstrap answers on 9101
#   3. ops/cci-config.sh          - a validated /etc/tai-talea/tai-talea.yaml
#
# Three things here are load-bearing, not style:
#
# 1. The Router listens on 127.0.0.1:8001, not 0.0.0.0:8000. 8000 belongs to
#    the nginx door (deploy/alayanew/edge/), which is the only thing the
#    platform publishes. A Router on 8000 would either collide with the door or
#    - worse - be the unauthenticated surface itself.
#
# 2. --health-check-endpoint /server_info. sglang's /health is a real 1-token
#    generation; under --disaggregation-mode=decode it is built with
#    bootstrap_host=FAKE_BOOTSTRAP_HOST and bootstrap_room=0, so the probe waits
#    for a prefill KV that never arrives and answers 503 after 20s. On
#    2026-10-26 that cascade marked the only decode worker unhealthy for 70
#    seconds and every caller got "No available decode workers".
#    Measured: /health = fixed 1.0s plus a generation, /server_info = 9ms.
#
# 3. Nothing may report success that did not happen. An old Router that
#    survived the kill and kept the port answers health checks for a process
#    that died on "Address already in use"; a control plane that failed during
#    recovery still runs. So every start below is followed by an assertion on
#    the *new* process: the listener's PID must be one this script spawned, the
#    Router must answer 200, the control plane must answer /readyz 200. Any
#    miss exits non-zero - a half-started plane must not look deployed.
set -u

fail() { printf '!! %s\n' "$*" >&2; exit 1; }

: "${TALEA_ADMIN_TOKEN:?set TALEA_ADMIN_TOKEN - the value cci-config.sh wrote into the config}"
TOKEN=$TALEA_ADMIN_TOKEN
API=${TALEA_CONTROL_API:-http://127.0.0.1:8787}
ROUTER_PORT=${TALEA_ROUTER_PORT:-8001}
ROUTER_WAIT=${TALEA_ROUTER_WAIT:-30}
PLANE_WAIT=${TALEA_PLANE_WAIT:-30}

echo "===== 0. preflight ====="
[ -x /root/sgl-model-gateway ] || fail "/root/sgl-model-gateway not found - unpack the runtime first"
[ -x /root/tai-talea ] || fail "/root/tai-talea not found - unpack the runtime first"
[ -f /etc/tai-talea/tai-talea.yaml ] || fail "/etc/tai-talea/tai-talea.yaml not found - run ops/cci-config.sh first"
ss -lnt 2>/dev/null | grep -qE ':(9001|9002) ' && \
  fail "something is listening on 9001/9002 - on CCI those ports are public internet"

echo "===== 1. router ====="
for pid in $(ps -eo pid,cmd | grep '[s]gl-model-gateway launch' | awk '{print $1}'); do
  kill "$pid" 2>/dev/null
done
sleep 3
mkdir -p /opt/tai-talea/logs
setsid nohup /root/sgl-model-gateway launch \
  --host 127.0.0.1 --port "$ROUTER_PORT" \
  --pd-disaggregation \
  --health-check-interval-secs 5 \
  --health-check-timeout-secs 5 \
  --health-check-endpoint /server_info \
  --health-failure-threshold 5 \
  --health-success-threshold 1 \
  --worker-startup-timeout-secs 1800 \
  --log-dir /opt/tai-talea/logs \
  </dev/null >/opt/tai-talea/logs/router.log 2>&1 &
ROUTER_PID=$!

# Health must come from the process this script just spawned. A listener owned
# by anyone else means the kill missed an old Router and the new one died on
# "Address already in use" - the historical false success.
OWNER=""
for _ in $(seq 1 "$ROUTER_WAIT"); do
  sleep 1
  OWNER=$(ss -lntp 2>/dev/null | grep ":$ROUTER_PORT " | grep -o 'pid=[0-9]*' | head -1 | cut -d= -f2)
  [ -n "$OWNER" ] && break
done
[ -n "$OWNER" ] || { tail -30 /opt/tai-talea/logs/router.log; fail "nothing is listening on :$ROUTER_PORT after ${ROUTER_WAIT}s"; }
[ "$OWNER" = "$ROUTER_PID" ] || fail "port $ROUTER_PORT is owned by pid $OWNER, not the Router this script started (pid $ROUTER_PID) - an old process survived the kill"
CODE=$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$ROUTER_PORT/health" || echo 000)
[ "$CODE" = "200" ] || fail "router /health answered $CODE, want 200"
echo "router: pid $ROUTER_PID owns :$ROUTER_PORT, /health 200"

echo "===== 2. control plane ====="
pkill -x tai-talea 2>/dev/null
sleep 2
LOG=/opt/tai-talea/logs/control-plane.log
[ -f "$LOG" ] && mv "$LOG" "$LOG.prev-$(date +%H%M%S)"
setsid nohup /root/tai-talea -config /etc/tai-talea/tai-talea.yaml \
  </dev/null >"$LOG" 2>&1 &
PLANE_PID=$!

READY=""
for _ in $(seq 1 "$PLANE_WAIT"); do
  sleep 1
  READY=$(curl -s -m 3 -o /dev/null -w '%{http_code}' "$API/readyz" 2>/dev/null || echo 000)
  [ "$READY" = "200" ] && break
done
[ "$READY" = "200" ] || { tail -30 "$LOG"; fail "control plane /readyz did not reach 200 within ${PLANE_WAIT}s (last: $READY)"; }
pgrep -x tai-talea >/dev/null || fail "readyz is 200 but no tai-talea process is running - refusing to report success"
echo "control plane: pid $PLANE_PID, /readyz 200"

echo "===== 3. first look (t+20s) ====="
sleep 12
curl -s -m 20 -H "Authorization: Bearer $TOKEN" "$API/v1/capacity/instances" | python3 -c '
import sys, json
d = json.load(sys.stdin)
rows = d.get("instances") if isinstance(d, dict) else d
print("instances:", len(rows or []))
for r in (rows or []):
    print("  %-14s %-10s %-9s role=%-7s attempts=%s/%s" % (
        r.get("id"), r.get("instance_state"), r.get("service_state"),
        r.get("role"), r.get("start_attempts"), r.get("prepare_attempts")))
'
echo "===== 4. planner ====="
curl -s -m 20 -H "Authorization: Bearer $TOKEN" "$API/v1/capacity/planner" | head -c 400; echo
echo "===== 5. router workers ====="
curl -s -m 10 "http://127.0.0.1:$ROUTER_PORT/workers" | head -c 400; echo
