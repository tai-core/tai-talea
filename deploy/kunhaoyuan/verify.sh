#!/bin/bash
# End-to-end check: the router answers, the model generates, the token log lands
# on disk and matches the spec.
#
# The pod-level view is a red herring here: a worker's readiness probe answers
# 200 from /bootstrap/health while the weights are still loading, so only a real
# completion proves the deployment works.
#
# usage: verify.sh [endpoint]
set -uo pipefail
export KUBECONFIG=${KUBECONFIG:-/root/.kube/config}

HERE=$(cd "$(dirname "$0")" && pwd)
NS=${TALEA_NAMESPACE:-kunhaoyuan}
API=${1:-${TALEA_API:-http://192.168.239.2:31280}}
MODEL=${TALEA_MODEL:-glm-5.3-flash}

fail=0

echo "--- models"
if curl -fsS -m 10 "$API/v1/models" | head -c 300; then
  echo
else
  echo "  the router did not answer /v1/models"; fail=1
fi

echo "--- one real completion"
body=$(printf '{"model":"%s","messages":[{"role":"user","content":"用一句话说明什么是KV cache"}],"max_tokens":2048}' "$MODEL")
reply=$(curl -s -m 300 -H 'Content-Type: application/json' -d "$body" "$API/v1/chat/completions")
echo "$reply" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except ValueError:
    print("  response is not JSON"); sys.exit(1)
if "error" in d:
    print("  error: %s" % d["error"]); sys.exit(1)
usage = d.get("usage") or {}
content = (d["choices"][0]["message"].get("content") or "").strip()
print("  usage: %s" % usage)
print("  content: %s" % (content[:70] + ("..." if len(content) > 70 else "")))
sys.exit(0 if usage.get("completion_tokens") else 1)
' || fail=1

echo
echo "--- token log on the node, checked against the spec"
bash "$HERE/ops/export-ailog.sh" 50 || fail=1

echo
if [ "$fail" -eq 0 ]; then
  echo "VERIFIED: the deployment answers, generates and logs"
else
  echo "NOT VERIFIED: see the failures above"
fi
exit "$fail"
