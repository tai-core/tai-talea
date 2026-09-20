#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="${TAI_TALEA_ROOT:-/opt/tai-talea}"
PROFILE="${TAI_TALEA_PROFILE:-/etc/tai-talea/profile.json}"
HOST="${TAI_TALEA_BOOTSTRAP_HOST:-0.0.0.0}"
PORT="${TAI_TALEA_BOOTSTRAP_PORT:-9001}"
TOKEN_FILE="${TAI_TALEA_TOKEN_FILE:-${ROOT}/run/bootstrap.token}"
LOG_DIR="${TAI_TALEA_LOG_DIR:-${ROOT}/logs}"
PYTHON="${TAI_TALEA_PYTHON:-${ROOT}/venv/bin/python}"

mkdir -p "${ROOT}/run" "${LOG_DIR}" "$(dirname "${PROFILE}")"

die() {
  printf 'talea-worker: %s\n' "$*" >&2
  exit 1
}

token() {
  local value="${TAI_TALEA_BOOTSTRAP_TOKEN:-}"
  [[ -n "${value}" ]] || die "TAI_TALEA_BOOTSTRAP_TOKEN is required"
  umask 077
  printf '%s' "${value}" > "${TOKEN_FILE}"
}

request() {
  # Keep control calls in stdlib Python so the image does not depend on curl
  # being present in a partner-provided base image.
  "${PYTHON}" - "$@" <<'PY'
import json
import os
import sys
import urllib.error
import urllib.request

action = sys.argv[1]
port = int(os.environ.get("TAI_TALEA_BOOTSTRAP_PORT", "9001"))
token = os.environ["TAI_TALEA_BOOTSTRAP_TOKEN"]
base = "http://127.0.0.1:%d" % port
headers = {"X-Bootstrap-Token": token, "Accept": "application/json"}

if action == "health":
    method, path, body = "GET", "/bootstrap/health", None
elif action == "stop":
    method, path = "POST", "/bootstrap/stop"
    body = json.dumps({"force": False, "reason": "container shutdown"}).encode()
    headers["Content-Type"] = "application/json"
else:
    raise SystemExit("unknown entrypoint action: %s" % action)

try:
    request = urllib.request.Request(base + path, data=body, headers=headers, method=method)
    with urllib.request.urlopen(request, timeout=float(os.environ.get("TAI_TALEA_HEALTH_TIMEOUT", "5"))) as response:
        payload = json.loads(response.read().decode("utf-8"))
except (OSError, urllib.error.URLError, urllib.error.HTTPError, ValueError) as error:
    print("bootstrap request failed: %s" % error, file=sys.stderr)
    raise SystemExit(1)

if action == "health" and payload.get("status") != "ok":
    print(json.dumps(payload), file=sys.stderr)
    raise SystemExit(1)
print(json.dumps(payload))
PY
}

start_role() {
  local role="${TALEA_ROLE:-}"
  [[ -n "${role}" ]] || return 0
  [[ "${role}" == "prefill" || "${role}" == "decode" ]] || die "TALEA_ROLE must be prefill, decode, or empty"
  local model_id="${TALEA_MODEL_ID:-glm5.2}"
  local model_path="${TALEA_MODEL_PATH:-${model_id}}"
  local service_port="${TAI_TALEA_SERVICE_PORT:-9002}"
  "${PYTHON}" - "${role}" "${model_id}" "${model_path}" "${service_port}" <<'PY'
import json
import os
import sys
import time
import urllib.error
import urllib.request

role, model_id, model_path, port = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
base = "http://127.0.0.1:%d" % int(os.environ.get("TAI_TALEA_BOOTSTRAP_PORT", "9001"))
headers = {"X-Bootstrap-Token": os.environ["TAI_TALEA_BOOTSTRAP_TOKEN"], "Content-Type": "application/json"}
extra_args = []
backend = os.environ.get("TALEA_TRANSFER_BACKEND", "").strip()
pd_port = os.environ.get("TALEA_PD_BOOTSTRAP_PORT", "").strip()
if backend:
    extra_args.append("--disaggregation-transfer-backend=" + backend)
if pd_port:
    extra_args.append("--disaggregation-bootstrap-port=" + pd_port)
payload = json.dumps({"role": role, "model_id": model_id, "model_path": model_path,
                     "port": port, "extra_args": extra_args}).encode()

for _ in range(int(os.environ.get("TAI_TALEA_START_WAIT_SECONDS", "90"))):
    try:
        health_req = urllib.request.Request(base + "/bootstrap/health", headers=headers)
        with urllib.request.urlopen(health_req, timeout=5) as response:
            if json.loads(response.read().decode()).get("status") == "ok":
                break
    except (OSError, urllib.error.URLError, urllib.error.HTTPError, ValueError):
        time.sleep(1)
else:
    raise SystemExit("bootstrap did not become reachable")

request = urllib.request.Request(base + "/bootstrap/start", data=payload, headers=headers, method="POST")
try:
    with urllib.request.urlopen(request, timeout=30) as response:
        print(response.read().decode("utf-8"))
except urllib.error.HTTPError as error:
    # A restart can race with a previous process. Treat an already-running
    # matching service as success; any other error must fail the pod.
    body = error.read().decode("utf-8", errors="replace")
    try:
        status_req = urllib.request.Request(base + "/bootstrap/status", headers=headers)
        with urllib.request.urlopen(status_req, timeout=5) as response:
            status = json.loads(response.read().decode())
        if status.get("phase") == "RUNNING" and status.get("role") == role and status.get("model_id") == model_id:
            print(json.dumps(status))
            raise SystemExit(0)
    except (OSError, urllib.error.URLError, urllib.error.HTTPError, ValueError):
        pass
    raise SystemExit("bootstrap start failed (HTTP %s): %s" % (error.code, body))
PY
}

case "${1:-serve}" in
  health)
    request health
    ;;
  stop)
    request stop || true
    ;;
  serve)
    token
    [[ -z "${TALEA_GPU_ARCH:-}" ]] || printf 'talea-worker: GPU architecture=%s\n' "${TALEA_GPU_ARCH}" >&2
    [[ -z "${TALEA_CONTROL_ENDPOINT:-}" ]] || printf 'talea-worker: control endpoint=%s\n' "${TALEA_CONTROL_ENDPOINT}" >&2
    bootstrap_pid=0
    shutdown() {
      request stop >/dev/null 2>&1 || true
      if [[ "${bootstrap_pid}" -gt 0 ]]; then
        kill -TERM "${bootstrap_pid}" 2>/dev/null || true
      fi
    }
    trap shutdown TERM INT
    "${PYTHON}" -m tai_talea_bootstrap serve \
      --profile "${PROFILE}" \
      --host "${HOST}" \
      --port "${PORT}" \
      --token-file "${TOKEN_FILE}" \
      --log-dir "${LOG_DIR}" \
      --stop-timeout "${TAI_TALEA_STOP_TIMEOUT:-60}" &
    bootstrap_pid=$!
    for _ in $(seq 1 "${TAI_TALEA_BOOTSTRAP_WAIT_SECONDS:-30}"); do
      if ! kill -0 "${bootstrap_pid}" 2>/dev/null; then
        wait "${bootstrap_pid}" || true
        die "bootstrap exited before becoming ready"
      fi
      if request health >/dev/null 2>&1; then break; fi
      sleep 1
    done
    request health >/dev/null 2>&1 || die "bootstrap did not become ready"
    start_role
    wait "${bootstrap_pid}"
    ;;
  *)
    die "usage: entrypoint.sh [serve|health|stop]"
    ;;
esac
