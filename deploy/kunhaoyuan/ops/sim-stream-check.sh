#!/usr/bin/env bash
#
# Check the two streaming-metering fixes without a GPU.
#
# The router is pointed at llm-d-inference-sim, a Go fake backend that mimics
# vLLM's OpenAI surface, including its rule that a stream only carries usage
# when the request asks for it (pkg/endpoint/request.go: `sendUsageData = true;
# if IsStream() { sendUsageData = IncludeUsage() }`). That rule is the whole
# reason three of the four bad log rows existed, so the shape reproduces here.
#
# Four cases, each asserting what lands in the router's ai_log:
#
#   1  no --force-stream-usage, client did not ask   -> no tokens (the bug)
#   2  no --force-stream-usage, client asked         -> tokens present
#   3  --force-stream-usage,  client did not ask     -> tokens present
#   4  --force-stream-usage, upstream omits total    -> total = input + output
#
# Case 4 needs a backend that leaves total_tokens out, which the simulator
# never does, so a pass-through proxy that deletes just that field sits in
# front of it. Nothing else in the response is touched.
#
# What this cannot check: /generate. The simulator registers that route only
# under its vllm engine, at /inference/v1/generate, and answers with a `usage`
# object rather than sglang's `meta_info` - so it is not a stand-in for the
# route that produced L114.
#
# usage: SIM_BIN=/path/sim-linux ROUTER_BIN=/path/sgl-model-gateway ./sim-stream-check.sh
#
set -uo pipefail

SIM_BIN="${SIM_BIN:-/root/simtest/sim-linux}"
ROUTER_BIN="${ROUTER_BIN:-/root/builds/sglang-router-target/release/sgl-model-gateway}"
STRIP_TOTAL="${STRIP_TOTAL:-$(cd "$(dirname "$0")" && pwd)/strip_total.py}"
MODEL="${MODEL:-testmodel}"
SIM_PORT="${SIM_PORT:-8080}"
SHIM_PORT="${SHIM_PORT:-8090}"
ROUTER_PORT="${ROUTER_PORT:-30000}"
WORKDIR="${WORKDIR:-/tmp/sim-stream-check}"

FAILED=0
mkdir -p "$WORKDIR"

for f in "$SIM_BIN" "$ROUTER_BIN" "$STRIP_TOTAL"; do
  [ -e "$f" ] || { echo "missing: $f" >&2; exit 2; }
done

say() { printf '%s\n' "$*"; }
ok()  { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILED=1; }

stop_all() {
  pkill -f "$(basename "$SIM_BIN")" 2>/dev/null
  pkill -f 'strip_total.py' 2>/dev/null
  pkill -f "$(basename "$ROUTER_BIN")" 2>/dev/null
}

wait_for() {
  local url="$1" tries="${2:-60}"
  for _ in $(seq 1 "$tries"); do
    [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$url" 2>/dev/null)" = "200" ] && return 0
    sleep 0.4
  done
  return 1
}

start_sim() {
  nohup "$SIM_BIN" --model "$MODEL" --port "$SIM_PORT" --mode random \
    > "$WORKDIR/sim.log" 2>&1 &
  wait_for "http://127.0.0.1:$SIM_PORT/health" || { say "simulator did not come up"; exit 1; }
}

start_shim() {
  nohup python3 "$STRIP_TOTAL" "$SHIM_PORT" "$SIM_PORT" > "$WORKDIR/shim.log" 2>&1 &
  wait_for "http://127.0.0.1:$SHIM_PORT/health" || { say "shim did not come up"; exit 1; }
}

# $1 = extra router args, $2 = worker port, $3 = log file
start_router() {
  nohup "$ROUTER_BIN" launch --worker-urls "http://127.0.0.1:$2" \
    --port "$ROUTER_PORT" --enable-ai-access-log $1 > "$3" 2>&1 &
  wait_for "http://127.0.0.1:$ROUTER_PORT/health" || { say "router did not come up"; exit 1; }
  sleep 2
}

# $1 = output file, $2 = stream_options fragment (may be empty)
hit() {
  curl -N -sS --max-time 60 -o "$1" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL\",\"stream\":true,\"max_tokens\":6,\"messages\":[{\"role\":\"user\",\"content\":\"count to six\"}]$2}" \
    "http://127.0.0.1:$ROUTER_PORT/v1/chat/completions"
}

# Print each inner ai_log object from a router log, one per line.
ai_logs() {
  python3 - "$1" <<'PY'
import json, sys
for line in open(sys.argv[1], encoding='utf-8', errors='replace'):
    if '"ai_log"' not in line:
        continue
    try:
        outer = json.loads(line)
    except ValueError:
        continue
    inner = outer.get('ai_log')
    if isinstance(inner, str):
        try:
            inner = json.loads(inner)
        except ValueError:
            continue
    if isinstance(inner, dict):
        print(json.dumps(inner, ensure_ascii=False))
PY
}

# Number of usage objects the client received (as opposed to "usage":null).
usage_objects() { grep -c '"usage":{' "$1" 2>/dev/null || true; }

# Pull one field out of the last ai_log line of a log file.
last_field() {
  ai_logs "$1" | tail -1 | python3 -c 'import json,sys; d=json.loads(sys.stdin.read() or "{}"); v=d.get(sys.argv[1]); print("" if v is None else v)' "$2"
}

stop_all
sleep 1
start_sim
say "simulator up on :$SIM_PORT (model=$MODEL)"

say ""
say "== case 1: router without --force-stream-usage, client did not ask =="
start_router "" "$SIM_PORT" "$WORKDIR/r1.log"
hit "$WORKDIR/c1.sse" ""
if [ -n "$(last_field "$WORKDIR/r1.log" total_token)" ]; then
  bad "ai_log carried a total for a stream that never asked for usage"
else
  ok "ai_log has no token triple - this is the shape that cost 3 rows"
fi
[ "$(usage_objects "$WORKDIR/c1.sse")" = "0" ] \
  && ok "client stream carried no usage object" \
  || bad "client stream carried a usage object nobody asked for"

say ""
say "== case 2: router without --force-stream-usage, client asked =="
hit "$WORKDIR/c2.sse" ',"stream_options":{"include_usage":true}'
t=$(last_field "$WORKDIR/r1.log" total_token)
if [ -n "$t" ]; then
  ok "ai_log total_token=$t (input=$(last_field "$WORKDIR/r1.log" input_token) output=$(last_field "$WORKDIR/r1.log" output_token))"
else
  bad "ai_log still has no total although the client asked for usage"
fi
[ "$(usage_objects "$WORKDIR/c2.sse")" = "1" ] \
  && ok "client stream carried exactly one usage object" \
  || bad "client stream carried $(usage_objects "$WORKDIR/c2.sse") usage objects, expected 1"

stop_router_only() { pkill -f "$(basename "$ROUTER_BIN")" 2>/dev/null; sleep 2; }
stop_router_only

say ""
say "== case 3: router with --force-stream-usage, client did not ask =="
start_router "--force-stream-usage" "$SIM_PORT" "$WORKDIR/r2.log"
hit "$WORKDIR/c3.sse" ""
t=$(last_field "$WORKDIR/r2.log" total_token)
if [ -n "$t" ]; then
  ok "ai_log total_token=$t - the forced flag reached the backend"
else
  bad "the forced flag did not reach the backend, ai_log is still empty"
fi
[ "$(usage_objects "$WORKDIR/c3.sse")" = "1" ] \
  && ok "client stream also gained the trailing usage chunk (documented side effect)" \
  || bad "client stream has no usage object - the extra chunk is being stripped somewhere"
stop_router_only

say ""
say "== case 4: --force-stream-usage, upstream leaves total_tokens out =="
start_shim
start_router "--force-stream-usage" "$SHIM_PORT" "$WORKDIR/r3.log"
hit "$WORKDIR/c4.sse" ',"stream_options":{"include_usage":true}'
in=$(last_field "$WORKDIR/r3.log" input_token)
out=$(last_field "$WORKDIR/r3.log" output_token)
tot=$(last_field "$WORKDIR/r3.log" total_token)
upstream_usage=$(grep -o '"usage":{[^}]*}' "$WORKDIR/c4.sse" | head -1)
say "  upstream sent: $upstream_usage"
if [ -n "$in" ] && [ -n "$out" ] && [ "$tot" = "$((in + out))" ]; then
  ok "ai_log recomputed total_token=$tot from $in+$out"
else
  bad "ai_log total_token='$tot' is not input=$in + output=$out"
fi
case "$upstream_usage" in
  *total_tokens*) bad "the shim did not actually strip total_tokens, case 4 proved nothing" ;;
  *) ok "client response left untouched - the repair is log-side only" ;;
esac

stop_all
say ""
if [ "$FAILED" = "0" ]; then
  say "all checks passed"
else
  say "some checks failed"
fi
exit "$FAILED"
