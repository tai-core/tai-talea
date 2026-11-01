#!/bin/bash
# taleactl.sh - start and stop the GPU half of the talea deployment on a schedule.
#
# The router and the control plane are left running: neither holds a card, and
# keeping them up preserves the worker ids the router handed out. Only the four
# GPU workers are scaled, which is what returns the cards to the scheduler.
#
# Why patch instead of `kubectl scale`: this account is not granted the scale
# subresource (403), while a merge patch on the deployment is allowed.
#
# Why a real inference request instead of pod readiness: the worker's readiness
# probe is /bootstrap/health, and that endpoint answers 200 as long as the
# bootstrap process is up - it returns {"status":"ok","phase":"starting"} while
# the model is still loading. A "4/4 Ready" cluster can therefore serve nothing.
#
# `down` first closes the Router's routing to the workers and waits for in-flight
# requests to finish, so a scale-down does not truncate them. The requests that
# arrive during that window still get 503 - the pool is going away - but they get
# an honest 503 instead of a connection to a process that is already stopping.
# TALEA_SKIP_DRAIN=1 scales without that step.
#
# usage: taleactl.sh up|down|drain|undrain|status|hold [hours]|release

set -uo pipefail
export KUBECONFIG=${KUBECONFIG:-/root/.kube/config}
HERE=$(cd "$(dirname "$0")" && pwd)
NS=${NS:-kunhaoyuan}
WORKERS=${WORKERS:-"talea-w0 talea-w1 talea-w2 talea-w3"}
NODE_PORT=${NODE_PORT:-31280}
API=${API:-http://192.168.239.2:$NODE_PORT}
MODEL=${MODEL:-glm-5.3-flash}
HOLD_FILE=${HOLD_FILE:-/root/talea-hold-until}
LOCK_FILE=${LOCK_FILE:-/root/.taleactl.lock}

POD_READY_TIMEOUT=${POD_READY_TIMEOUT:-2400}   # 40 min: a cold load is 11-12 min
POD_GONE_TIMEOUT=${POD_GONE_TIMEOUT:-900}      # 15 min
SERVE_TIMEOUT=${SERVE_TIMEOUT:-2400}
QUIESCE_TIMEOUT=${QUIESCE_TIMEOUT:-90}         # how long in-flight may take to finish

log() { printf '%s %s\n' "$(date '+%F %T')" "$*"; }
die() { log "FAILED: $*"; exit 1; }

# One run at a time. A 20:00 up that is still loading when someone fires `down`
# by hand must not interleave patches.
if command -v flock >/dev/null 2>&1; then
  exec 9>"$LOCK_FILE" || exit 1
  if ! flock -n 9; then
    log "another taleactl run holds the lock; nothing to do"
    exit 0
  fi
fi

set_replicas() { # $1=count, rest=deployment names
  local n=$1; shift
  local d
  for d in "$@"; do
    kubectl -n "$NS" patch deploy "$d" --type=merge \
      -p "{\"spec\":{\"replicas\":$n}}" >/dev/null || return 1
  done
}

replicas_of() { # $1=deployment
  local v
  v=$(kubectl -n "$NS" get deploy "$1" -o jsonpath='{.spec.replicas}' 2>/dev/null)
  echo "${v:-0}"
}

ready_of() { # $1=deployment
  local v
  v=$(kubectl -n "$NS" get deploy "$1" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
  echo "${v:-0}"
}

worker_pods() {
  kubectl -n "$NS" get pods --no-headers 2>/dev/null |
    awk -v list="$WORKERS" '
      BEGIN { n = split(list, a, " "); for (i = 1; i <= n; i++) want[a[i]] = 1 }
      { for (p in want) if ($1 ~ ("^" p "-")) { print $1, $2, $3; break } }
    '
}

# A pod can report 1/1 while it is already Terminating, so phase matters too.
ready_count() {
  worker_pods | awk '$2 == "1/1" && $3 == "Running" { c++ } END { print c + 0 }'
}

hold_active() {
  [ -f "$HOLD_FILE" ] || return 1
  local until
  until=$(cat "$HOLD_FILE" 2>/dev/null)
  if [ -z "$until" ] || [ "$until" -lt "$(date +%s)" ] 2>/dev/null; then
    rm -f "$HOLD_FILE"
    return 1
  fi
  log "hold is active until $(date -d "@$until" '+%F %T')"
  return 0
}

wait_pods_ready() {
  local deadline=$(( $(date +%s) + POD_READY_TIMEOUT )) want
  want=$(echo "$WORKERS" | wc -w)
  while [ "$(date +%s)" -lt "$deadline" ]; do
    local ready; ready=$(ready_count)
    log "pods ready: $ready/$want"
    [ "$ready" -ge "$want" ] && return 0
    sleep 15
  done
  return 1
}

wait_pods_gone() {
  local deadline=$(( $(date +%s) + POD_GONE_TIMEOUT )) left
  while [ "$(date +%s)" -lt "$deadline" ]; do
    left=$(worker_pods | wc -l)
    log "worker pods still present: $left"
    [ "$left" -eq 0 ] && return 0
    sleep 10
  done
  return 1
}

# The only signal that proves the model is actually answering.
probe_inference() {
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' -m 120 \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":8}" \
    "$API/v1/chat/completions" 2>/dev/null)
  [ "$code" = "200" ]
}

wait_inference() {
  local deadline=$(( $(date +%s) + SERVE_TIMEOUT ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if probe_inference; then
      log "inference answered 200"
      return 0
    fi
    log "inference not ready yet"
    sleep 20
  done
  return 1
}

do_up() {
  hold_active && { log "up suppressed by hold"; return 0; }
  log "up: scaling ${WORKERS} to 1"
  set_replicas 1 $WORKERS || die "could not patch the worker deployments"

  wait_pods_ready || die "workers did not become Ready in ${POD_READY_TIMEOUT}s"
  wait_inference || die "the endpoint never answered a request in ${SERVE_TIMEOUT}s"

  log "up: complete, $(ready_count)/$(echo "$WORKERS" | wc -w) pods ready"
  do_status
}

# Close routing before the Pods disappear, so that what fails during a
# scale-down is only the requests that arrive after there is no capacity left,
# not the ones already in flight.
#
# This runs as a Pod because the Router's control port is a ClusterIP, and the
# jump host can reach neither that nor any Pod address. Best effort on purpose:
# at 08:00 the cards have to come back either way, so a failed drain is logged
# and the scale-down proceeds.
do_drain() { # $1 = close (default) | open
  local image pod=talea-drain rc mode=${1:-close}
  image=$(kubectl -n "$NS" get deploy talea-router -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)
  [ -n "$image" ] || { log "drain skipped: cannot read the router image"; return 0; }

  kubectl -n "$NS" create configmap talea-drain-script \
    --from-file=drain.py="$HERE/drain-to-router.py" --dry-run=client -o yaml |
    kubectl apply -f - >/dev/null || { log "drain skipped: could not stage the script"; return 0; }

  kubectl -n "$NS" delete pod "$pod" --ignore-not-found --wait=true >/dev/null 2>&1
  kubectl -n "$NS" apply -f - >/dev/null <<YAML || { log "drain skipped: could not schedule the Pod"; return 0; }
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  namespace: $NS
spec:
  restartPolicy: Never
  imagePullSecrets:
    - name: harbor-cred
  containers:
    - name: drain
      image: $image
      imagePullPolicy: IfNotPresent
      command: ["/bin/bash", "-c"]
      args:
        - |
          py=\$(command -v python3 || command -v /opt/sglang/bin/python3 || echo /opt/sglang/bin/python)
          exec "\$py" /drain/drain.py
      env:
        - {name: ROUTER_URL, value: "http://talea-router:30001"}
        - {name: QUIESCE_TIMEOUT, value: "$QUIESCE_TIMEOUT"}
        - {name: MODE, value: "$mode"}
      volumeMounts:
        - {name: script, mountPath: /drain, readOnly: true}
  volumes:
    - name: script
      configMap:
        name: talea-drain-script
YAML

  local deadline=$(( $(date +%s) + QUIESCE_TIMEOUT + 120 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    case "$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)" in
      Succeeded) rc=0; break ;;
      Failed)    rc=1; break ;;
    esac
    sleep 3
  done
  kubectl -n "$NS" logs "$pod" 2>&1 | sed 's/^/  /'
  kubectl -n "$NS" delete pod "$pod" --wait=false >/dev/null 2>&1
  return "${rc:-1}"
}

do_down() {
  hold_active && { log "down suppressed by hold"; return 0; }

  if [ "${TALEA_SKIP_DRAIN:-0}" = "1" ]; then
    log "down: drain skipped on request"
  else
    log "down: closing routing to the workers"
    if do_drain close; then
      log "down: routing closed and no request is in flight"
    else
      log "WARNING: drain did not complete cleanly; scaling anyway to release the cards"
    fi
  fi

  log "down: scaling ${WORKERS} to 0"
  set_replicas 0 $WORKERS || die "could not patch the worker deployments"

  # Blocking here is the point: the cards are only free once the containers
  # have exited, and sglang needs its 60 s SIGTERM window to stop cleanly.
  if wait_pods_gone; then
    log "down: complete, the cards are back in the scheduler"
  else
    # Not fatal: a Pod stuck in Terminating is the platform's problem, and the
    # cards are released when it finally goes. Say so instead of hiding it.
    log "WARNING: worker pods are still present after ${POD_GONE_TIMEOUT}s"
    worker_pods | sed 's/^/  /'
  fi
  do_status
}

do_status() {
  local d
  echo "--- deployments"
  for d in $WORKERS; do
    printf '  %-9s replicas=%s ready=%s\n' "$d" "$(replicas_of "$d")" "$(ready_of "$d")"
  done
  printf '  %-9s ready=%s/1\n' "router" "$(ready_of talea-router)"
  printf '  %-9s ready=%s/1\n' "control" "$(ready_of talea-control)"

  echo "--- worker pods"
  worker_pods | sed 's/^/  /' || true

  echo "--- cards requested by these pods"
  kubectl -n "$NS" get pods -o json 2>/dev/null | python3 -c '
import json, sys
data = json.load(sys.stdin)
names = set(sys.argv[1].split())
total = 0
# Terminating pods are counted on purpose: the container only releases the
# cards when it exits, so a pod on its way out still holds them.
for pod in data.get("items", []):
    name = pod["metadata"]["name"]
    if not any(name.startswith(n + "-") for n in names):
        continue
    if pod.get("status", {}).get("phase") in ("Succeeded", "Failed"):
        continue
    for c in pod["spec"]["containers"]:
        limits = (c.get("resources") or {}).get("limits") or {}
        gpu = limits.get("nvidia.com/gpu")
        if gpu:
            total += int(gpu)
print("  %d" % total)
' "$WORKERS"

  echo "--- serving instances (control plane log, advisory)"
  kubectl -n "$NS" logs deploy/talea-control --since=45m 2>/dev/null |
    grep -c "service serving" | sed 's/^/  reached serving in the last 45m: /'

  echo "--- endpoint"
  if probe_inference; then
    echo "  $API/v1/chat/completions -> 200"
  else
    echo "  $API/v1/chat/completions -> not answering"
  fi
}

do_hold() {
  local hours=${1:-2}
  local until=$(( $(date +%s) + hours * 3600 ))
  echo "$until" > "$HOLD_FILE"
  log "hold set for ${hours}h, until $(date -d "@$until" '+%F %T')"
}

do_release() {
  rm -f "$HOLD_FILE"
  log "hold released"
}

case "${1:-}" in
  up)      do_up ;;
  down)    do_down ;;
  drain)   do_drain close ;;
  undrain) do_drain open ;;
  status)  do_status ;;
  hold)    do_hold "${2:-2}" ;;
  release) do_release ;;
  *)
    sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac
