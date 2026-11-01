#!/bin/bash
# Copy the router's token log off the node and check it against the spec.
#
# The log lives on a hostPath, and the account has neither pods/exec nor
# port-forward, so the only way to read node-local disk is a read-only Pod that
# mounts it and prints to stdout. Same technique the glm53 stack used.
#
# usage: export-ailog.sh [lines]     (default 500 of the most recent token lines)
set -uo pipefail
export KUBECONFIG=${KUBECONFIG:-/root/.kube/config}
HERE=$(cd "$(dirname "$0")" && pwd)
NS=${TALEA_NAMESPACE:-kunhaoyuan}
POD=talea-ailog-export
LINES=${1:-500}
OUT=${OUT:-/root/talea-ai-access.jsonl}
TMPL=${TMPL:-$HERE/ailog-export-pod.yaml.tmpl}
API=${API:-http://192.168.239.2:31280}

RNODE=$(kubectl -n $NS get pod -l app=talea-router -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)
[ -z "$RNODE" ] && { echo "the router pod is not running; nothing to export"; exit 1; }
echo "router node = $RNODE"

kubectl -n $NS delete pod $POD --ignore-not-found --wait=true >/dev/null 2>&1
sed -e "s|__NODE__|$RNODE|" -e "s|__LINES__|$LINES|" "$TMPL" | kubectl apply -f - >/dev/null || exit 1

for i in $(seq 1 40); do
  ph=$(kubectl -n $NS get pod $POD -o jsonpath='{.status.phase}' 2>/dev/null)
  case "$ph" in Succeeded|Failed) break ;; esac
  sleep 3
done
kubectl -n $NS logs $POD > /root/ailog-dump.txt 2>&1
kubectl -n $NS delete pod $POD --wait=false >/dev/null 2>&1

echo "--- node side"
sed -n '/@@@ NODE/p;/@@@ SIZE/p;/@@@ COUNT/p;/@@@ TOTAL_LINES/p;/@@@ MISSING/p' /root/ailog-dump.txt

sed -n '/@@@ AI_BEGIN/,/@@@ AI_END/p' /root/ailog-dump.txt | sed '1d;$d' > "$OUT"
echo "--- exported $(wc -l < "$OUT") line(s) to $OUT"

if [ ! -s "$OUT" ]; then
  echo "no token lines yet: drive one request through $API and retry"
  exit 0
fi

echo "--- first line"
head -1 "$OUT"
echo "--- last line"
tail -1 "$OUT"

echo
echo "=== spec conformance ==="
python3 "$HERE/check-ailog.py" "$OUT"
