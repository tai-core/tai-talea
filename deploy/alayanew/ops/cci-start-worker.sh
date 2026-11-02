#!/bin/bash
# Start the worker-side node manager (tai_talea_bootstrap) on port 9101.
#
# This is the step the rebuild flow used to miss entirely: the control plane
# dispatches starts to http://<worker>:9101/bootstrap/start, so a worker
# without a running, correctly-tokened bootstrap can only ever answer 401 (or
# nothing at all), no matter how healthy its venv is. Run this on BOTH workers
# before starting the control plane.
#
#   TALEA_BOOTSTRAP_TOKEN   required - the exact value cci-config.sh wrote
#                           into the control plane's controller.bootstrap_token
#   TALEA_BOOTSTRAP_PORT    optional, default 9101
#   TALEA_PROFILE           optional, default /etc/tai-talea/profile.json
#   TALEA_TOKEN_FILE        optional, default /etc/tai-talea/bootstrap-token
#
# Checks before starting: profile and bootstrap source exist, ports 9001/9002
# have no listener (they are CCI's force-published system ports - anything
# listening there is on the public internet), and no live bootstrap is already
# serving. Checks after: 9101 answers with the token and refuses a wrong one.
set -u

die() { printf '!! %s\n' "$*" >&2; exit 1; }

: "${TALEA_BOOTSTRAP_TOKEN:?set TALEA_BOOTSTRAP_TOKEN - the same value the control plane was configured with}"

BOOTSTRAP_PORT=${TALEA_BOOTSTRAP_PORT:-9101}
PROFILE=${TALEA_PROFILE:-/etc/tai-talea/profile.json}
TOKEN_FILE=${TALEA_TOKEN_FILE:-/etc/tai-talea/bootstrap-token}
ROOT=/opt/tai-talea
BOOTSTRAP_SRC=${TALEA_BOOTSTRAP_SRC:-$ROOT/bootstrap-src}
LOG_DIR=${TALEA_LOG_DIR:-$ROOT/logs}
PID_FILE=${TALEA_PID_FILE:-$ROOT/run/bootstrap.pid}
PYTHON=${TALEA_BOOTSTRAP_PYTHON:-python3}

# ---------------------------------------------------------------- preflight
[ -f "$PROFILE" ] || die "profile $PROFILE not found - unpack the runtime first (ops/cci-unpack-runtime.sh)"
[ -d "$BOOTSTRAP_SRC/tai_talea_bootstrap" ] || die "bootstrap source $BOOTSTRAP_SRC/tai_talea_bootstrap not found"
command -v "$PYTHON" >/dev/null 2>&1 || die "$PYTHON not found"

# The two ports the platform force-publishes must stay unbound: a listener on
# 9001/9002 is a listener on the public internet, and that is the whole
# failure mode this port move exists to prevent. The override is an escape
# hatch for platforms other than CCI, nothing else.
if [ "${TALEA_ALLOW_LEGACY_PORTS:-0}" != "1" ]; then
  if ss -lnt 2>/dev/null | grep -qE ':(9001|9002) '; then
    die "something is listening on 9001/9002 - on CCI those are public; find and stop it before starting the bootstrap"
  fi
fi

# ------------------------------------------------------- replace an old one
mkdir -p "$LOG_DIR" "$(dirname "$PID_FILE")"
if [ -f "$PID_FILE" ]; then
  pid=$(cat "$PID_FILE" 2>/dev/null || true)
  if [ -n "$pid" ] && [ -r "/proc/$pid/cmdline" ]; then
    if tr '\0' ' ' < "/proc/$pid/cmdline" | grep -q 'tai_talea_bootstrap serve'; then
      echo "stopping the previous bootstrap (pid $pid)"
      kill "$pid" 2>/dev/null || true
      for _ in $(seq 1 100); do
        [ -e "/proc/$pid" ] || break
        sleep 0.1
      done
      [ -e "/proc/$pid" ] && die "the previous bootstrap (pid $pid) did not exit"
    fi
  fi
  rm -f "$PID_FILE"
fi

# ------------------------------------------------------- token, then start
# The token file is the worker's only credential; it must match the control
# plane's controller.bootstrap_token or every start request is a 401.
umask 077
printf '%s' "$TALEA_BOOTSTRAP_TOKEN" > "$TOKEN_FILE"
chmod 600 "$TOKEN_FILE" 2>/dev/null || true

echo "starting the bootstrap on 0.0.0.0:$BOOTSTRAP_PORT"
setsid nohup env PYTHONPATH="$BOOTSTRAP_SRC" "$PYTHON" -m tai_talea_bootstrap serve \
  --profile "$PROFILE" \
  --host 0.0.0.0 \
  --port "$BOOTSTRAP_PORT" \
  --token-file "$TOKEN_FILE" \
  --log-dir "$LOG_DIR" \
  </dev/null >>"$LOG_DIR/bootstrap.log" 2>&1 &
echo $! > "$PID_FILE"

# ------------------------------------------------------------------ verify
sleep 3
ok=$(curl -s -m 5 -o /dev/null -w '%{http_code}' \
  -H "X-Bootstrap-Token: $TALEA_BOOTSTRAP_TOKEN" \
  "http://127.0.0.1:$BOOTSTRAP_PORT/bootstrap/status" || echo 000)
[ "$ok" = "200" ] || { tail -20 "$LOG_DIR/bootstrap.log"; die "the bootstrap did not answer 200 on :$BOOTSTRAP_PORT (got $ok)"; }

bad=$(curl -s -m 5 -o /dev/null -w '%{http_code}' \
  -H "X-Bootstrap-Token: wrong-token" \
  "http://127.0.0.1:$BOOTSTRAP_PORT/bootstrap/status" || echo 000)
[ "$bad" = "401" ] || die "a wrong token got HTTP $bad - the bootstrap must reject it with 401"

echo "ok: bootstrap healthy on :$BOOTSTRAP_PORT, wrong token rejected with 401, pid $(cat "$PID_FILE")"
echo "next: repeat on the other worker, then start the control plane (ops/cci-start-plane.sh)"
