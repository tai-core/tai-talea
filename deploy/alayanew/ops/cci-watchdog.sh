#!/bin/bash
# cci-watchdog.sh - supervise the tai-talea processes and rotate the logs.
#
# Why this exists: every tai-talea process on a CCI instance runs under
# `setsid nohup` with no supervision at all. The image's PID 1 is s6-svscan,
# but systemd reports "offline", there is no cron and no logrotate, and
# router.log grows at ~130 MB/day. One script, started by cron (or by hand),
# covers both gaps:
#
#   - supervision: a dead control plane or Router is restarted in place
#     (surgically - a healthy Router is never bounced just because the plane
#     died); a dead worker bootstrap likewise;
#   - rotation: every log over the size cap is copytruncate-rotated and
#     gzipped, old generations pruned, and the smg.* dated logs expire after
#     three days.
#
# usage:
#   cci-watchdog.sh loop [interval_s]   - supervise forever (default 30 s)
#   cci-watchdog.sh once                - one pass; this is what cron runs
#
# Touch /opt/tai-talea/run/watchdog.pause to suspend supervision (rotation
# continues) - for deliberate maintenance stops that must not be "healed".

set -u

LOG_DIR=${TALEA_LOG_DIR:-/opt/tai-talea/logs}
RUN_DIR=${TALEA_RUN_DIR:-/opt/tai-talea/run}
PAUSE_FILE=$RUN_DIR/watchdog.pause
SIZE_CAP_MB=${TALEA_LOG_CAP_MB:-200}
KEEP_GENS=${TALEA_LOG_KEEP:-2}
SMG_KEEP_DAYS=${TALEA_SMG_KEEP_DAYS:-3}
DISK_FORCE_PCT=${TALEA_DISK_FORCE_PCT:-85}
PLANE_BIN=${TALEA_PLANE_BIN:-/root/tai-talea}
CONFIG=${TALEA_CONFIG:-/etc/tai-talea/tai-talea.yaml}
START_PLANE_SH=${TALEA_START_PLANE:-/root/cci-start-plane.sh}
START_WORKER_SH=${TALEA_START_WORKER:-/root/cci-start-worker.sh}
ROUTER_PORT=${TALEA_ROUTER_PORT:-8001}
PLANE_API=${TALEA_CONTROL_API:-http://127.0.0.1:8787}
WATCHDOG_LOG=$LOG_DIR/watchdog.log

mode=${1:-loop}
case "$mode" in
  once|loop) ;;
  *) echo "usage: $0 [once|loop] [interval_s]" >&2; exit 2 ;;
esac

mkdir -p "$LOG_DIR" "$RUN_DIR"

say() { echo "$(date '+%F %T') $*" >>"$WATCHDOG_LOG"; }

# ---- single instance for loop mode ------------------------------------------
# A lock dir (not a pidfile) so a killed watchdog can never leave a stale
# lock behind: mkdir is atomic and dies with the holder only if we also
# verify the recorded pid.
acquire_lock() {
  local dir="$RUN_DIR/watchdog.lock"
  if mkdir "$dir" 2>/dev/null; then
    echo $$ >"$dir/pid"
    return 0
  fi
  local other
  other=$(cat "$dir/pid" 2>/dev/null || echo 0)
  if [ "$other" != "0" ] && ! kill -0 "$other" 2>/dev/null; then
    say "stale lock from dead pid $other - taking over"
    rmdir "$dir" 2>/dev/null
    mkdir "$dir" 2>/dev/null && { echo $$ >"$dir/pid"; return 0; }
  fi
  return 1
}

# ---- rotation ---------------------------------------------------------------
# copytruncate, not mv: the writers were started with `>>log` and hold the
# inode, so after a rename they would keep filling the rotated file. The
# window between cp and truncate can lose a few lines; that is the standard
# copytruncate trade and it is documented here, not hidden.
disk_pct() { df -P "$LOG_DIR" | awk 'NR==2 {gsub("%",""); print $5}'; }

rotate_one() {
  local file=$1 ts base
  ts=$(date +%Y%m%d-%H%M%S)
  base=${file%.log}
  cp -a "$file" "$base.$ts.log" || return 1
  truncate -s 0 "$file" || return 1
  nice gzip -f "$base.$ts.log" 2>/dev/null &
}

prune_gens() {
  # keep the newest $KEEP_GENS generations per log base name
  local base=$1
  ls -1t "$base".[0-9]*.log.gz 2>/dev/null | tail -n +$((KEEP_GENS + 1)) | xargs -r rm -f
}

rotate_logs() {
  local force=$1 file f size cap_bytes pct
  cap_bytes=$((SIZE_CAP_MB * 1024 * 1024))
  pct=$(disk_pct)
  [ "$pct" -ge "$DISK_FORCE_PCT" ] && force=1
  for file in "$LOG_DIR"/*.log; do
    [ -f "$file" ] || continue
    case "$file" in "$WATCHDOG_LOG") continue ;; esac
    size=$(stat -c%s "$file" 2>/dev/null || echo 0)
    if [ "$force" = 1 ] || [ "$size" -ge "$cap_bytes" ]; then
      rotate_one "$file" && say "rotated $(basename "$file") ($((size / 1024 / 1024)) MB, disk ${pct}%)"
    fi
    prune_gens "${file%.log}"
  done
  # the watchdog's own log, capped at a tenth of the others
  size=$(stat -c%s "$WATCHDOG_LOG" 2>/dev/null || echo 0)
  if [ "$size" -ge $((cap_bytes / 10)) ]; then
    local ts; ts=$(date +%Y%m%d-%H%M%S)
    cp -a "$WATCHDOG_LOG" "$WATCHDOG_LOG.$ts" && truncate -s 0 "$WATCHDOG_LOG"
    nice gzip -f "$WATCHDOG_LOG.$ts" 2>/dev/null &
    ls -1t "$WATCHDOG_LOG".[0-9]* 2>/dev/null | tail -n +$((KEEP_GENS + 1)) | xargs -r rm -f
  fi
  # smg.* dated logs: compress the big ones, expire after SMG_KEEP_DAYS
  for file in "$LOG_DIR"/smg.[0-9]*-*-*; do
    [ -f "$file" ] || continue
    if [ ! -f "$file.gz" ]; then
      size=$(stat -c%s "$file" 2>/dev/null || echo 0)
      [ "$size" -ge $((cap_bytes / 2)) ] && { nice gzip -f "$file" 2>/dev/null & say "gzipped $(basename "$file")"; }
    fi
  done
  find "$LOG_DIR" -name 'smg.*.gz' -mtime +$SMG_KEEP_DAYS -delete 2>/dev/null
  # crash-scattered .prev-* plane logs from cci-start-plane.sh: keep a week
  find "$LOG_DIR" -name 'control-plane.log.prev-*' -mtime +7 -delete 2>/dev/null
}

# ---- supervision -------------------------------------------------------------
router_healthy() {
  [ -n "$(ss -lntp 2>/dev/null | grep ":$ROUTER_PORT ")" ] || return 1
  [ "$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$ROUTER_PORT/health" 2>/dev/null)" = "200" ]
}

plane_running() { pgrep -x tai-talea >/dev/null; }

start_plane_only() {
  # The full cci-start-plane.sh restarts the Router too; when the Router is
  # healthy that is pure damage. Replicate just the plane block of that
  # script - same binary, same config, same log - and assert readyz like it.
  [ -x "$PLANE_BIN" ] && [ -f "$CONFIG" ] || { say "plane start skipped: missing $PLANE_BIN or $CONFIG"; return 1; }
  say "control plane is down (router healthy) - starting plane only"
  setsid nohup "$PLANE_BIN" -config "$CONFIG" </dev/null >>"$LOG_DIR/control-plane.log" 2>&1 &
  local i code
  for i in $(seq 1 30); do
    sleep 1
    code=$(curl -s -m 3 -o /dev/null -w '%{http_code}' "$PLANE_API/readyz" 2>/dev/null || echo 000)
    [ "$code" = "200" ] && { say "control plane restarted (readyz 200, pid $(pgrep -x tai-talea | head -1))"; return 0; }
  done
  say "control plane did not reach readyz 200 in 30 s (last $code)"
  return 1
}

start_everything() {
  say "router is down - running $START_PLANE_SH (starts router + plane)"
  if TALEA_ADMIN_TOKEN=$(awk '/admin_token:/ {print $2}' "$CONFIG" | head -1) \
     bash "$START_PLANE_SH" >>"$WATCHDOG_LOG" 2>&1; then
    say "router + control plane restarted"
  else
    say "cci-start-plane.sh failed - see its output above"
  fi
}

bootstrap_running() {
  local pid
  pid=$(cat "$RUN_DIR/bootstrap.pid" 2>/dev/null || echo 0)
  if [ "$pid" != "0" ] && kill -0 "$pid" 2>/dev/null; then return 0; fi
  pgrep -f 'tai_talea_bootstrap serve' >/dev/null
}

supervise_pass() {
  if [ -f /root/cci-start-plane.sh ] && [ -f "$CONFIG" ]; then
    if ! router_healthy; then
      start_everything
    elif ! plane_running; then
      start_plane_only
    fi
  elif [ -f "$START_WORKER_SH" ]; then
    if ! bootstrap_running; then
      say "worker bootstrap is down - running $START_WORKER_SH"
      bash "$START_WORKER_SH" >>"$WATCHDOG_LOG" 2>&1 && say "worker bootstrap restarted" \
        || say "cci-start-worker.sh failed"
    fi
  fi
}

# ---- main --------------------------------------------------------------------
if [ "$mode" = once ]; then
  rotate_logs 0
  supervise_pass
  exit 0
fi

INTERVAL=${2:-30}
acquire_lock || { echo "another watchdog (loop) already holds the lock" >&2; exit 0; }
say "watchdog loop started (pid $$, interval ${INTERVAL}s, cap ${SIZE_CAP_MB}MB)"
while :; do
  rotate_logs 0
  if [ -e "$PAUSE_FILE" ]; then
    say "pause file present - supervision suspended (rotation only)"
  else
    supervise_pass
  fi
  sleep "$INTERVAL"
done
