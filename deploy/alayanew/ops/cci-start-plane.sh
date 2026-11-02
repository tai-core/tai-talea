#!/bin/bash
# Start the Router and the control plane on the first CCI container, then
# report what the control plane sees after its first reconcile passes.
#
# Two things here are load-bearing, not style:
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
# Do not "improve" the kill below into `pkill -x sgl-model-gateway`: comm is
# truncated to 15 characters ("sgl-model-gatew"), the exact match hits nothing,
# the old Router keeps the port, the replacement dies on "Address already in
# use", and a probe answers 200 from the OLD process - a false success.
set -u
: "${TALEA_ADMIN_TOKEN:?set TALEA_ADMIN_TOKEN}"
TOKEN=$TALEA_ADMIN_TOKEN
API=http://127.0.0.1:8787
ROUTER_PORT=8001

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
sleep 6
curl -s -m 5 -o /dev/null -w "router /health http=%{http_code}\n" "http://127.0.0.1:$ROUTER_PORT/health"
OWNER=$(ss -lntp 2>/dev/null | grep ":$ROUTER_PORT " | grep -o 'pid=[0-9]*' | head -1 | cut -d= -f2)
echo "router listener pid=${OWNER:-none}"
curl -s -m 5 "http://127.0.0.1:$ROUTER_PORT/workers" | head -c 300; echo

echo "===== 2. control plane ====="
pkill -x tai-talea 2>/dev/null
sleep 2
LOG=/opt/tai-talea/logs/control-plane.log
[ -f "$LOG" ] && mv "$LOG" "$LOG.prev-$(date +%H%M%S)"
setsid nohup /root/tai-talea -config /etc/tai-talea/tai-talea.yaml \
  </dev/null >"$LOG" 2>&1 &
sleep 8
pgrep -x tai-talea >/dev/null && echo "control plane is up" || { echo "START FAILED"; tail -20 "$LOG"; exit 1; }
head -25 "$LOG"

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
