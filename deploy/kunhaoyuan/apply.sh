#!/bin/bash
# Render the secret-bearing files and apply the kunhaoyuan stack in order.
#
# Why rendering instead of committed manifests: config.yaml carries three shared
# tokens and the worker Pods carry the bootstrap token in an env var and in the
# probe headers. This repository is public, so the secrets live in one file on
# the jump host and the manifests are templates.
#
# usage: apply.sh [render|check|core|workers|all]      (default: all)
#
# Prerequisites, once per namespace:
#   kubectl -n <ns> create secret docker-registry harbor-cred \
#     --docker-server=<registry> --docker-username=<user> --docker-password=<pass>
# and a secrets file (see `apply.sh secrets`).
set -uo pipefail
export KUBECONFIG=${KUBECONFIG:-/root/.kube/config}

HERE=$(cd "$(dirname "$0")" && pwd)
NS=${TALEA_NAMESPACE:-kunhaoyuan}
SECRETS=${TALEA_SECRETS:-/root/.talea-secrets.env}
WORK=${TALEA_RENDER_DIR:-/root/talea-render}

log() { printf '%s %s\n' "$(date '+%F %T')" "$*"; }
die() { log "FAILED: $*"; exit 1; }

load_secrets() {
  [ -f "$SECRETS" ] || die "no $SECRETS; run '$0 secrets' and edit it"
  # shellcheck disable=SC1090
  set -a; . "$SECRETS"; set +a
  [ -n "${TALEA_BOOTSTRAP_TOKEN:-}" ] || die "$SECRETS does not set TALEA_BOOTSTRAP_TOKEN"
  [ -n "${TALEA_ADMIN_TOKEN:-}" ] || die "$SECRETS does not set TALEA_ADMIN_TOKEN"
  [ -n "${TALEA_PUSH_TOKEN:-}" ] || die "$SECRETS does not set TALEA_PUSH_TOKEN"
  [ -n "${TALEA_PUSH_SECRET:-}" ] || die "$SECRETS does not set TALEA_PUSH_SECRET"
}

do_secrets() {
  [ -f "$SECRETS" ] && die "$SECRETS already exists; refusing to overwrite tokens in use"
  umask 077
  cat > "$SECRETS" <<EOF
# Shared secrets for the kunhaoyuan deployment. Never commit this file.
TALEA_ADMIN_TOKEN=$(openssl rand -hex 16)
TALEA_BOOTSTRAP_TOKEN=$(openssl rand -hex 16)
TALEA_PUSH_TOKEN=$(openssl rand -hex 16)
TALEA_PUSH_SECRET=$(openssl rand -hex 16)
EOF
  log "wrote $SECRETS; keep it, every re-render reads it back"
}

do_render() {
  load_secrets
  mkdir -p "$WORK"; chmod 700 "$WORK"
  sed -e "s|__ADMIN_TOKEN__|$TALEA_ADMIN_TOKEN|g" \
      -e "s|__BOOTSTRAP_TOKEN__|$TALEA_BOOTSTRAP_TOKEN|g" \
      -e "s|__PUSH_TOKEN__|$TALEA_PUSH_TOKEN|g" \
      -e "s|__PUSH_SECRET__|$TALEA_PUSH_SECRET|g" \
      "$HERE/config.yaml.tmpl" > "$WORK/config.yaml" || die "could not render config.yaml"
  chmod 600 "$WORK/config.yaml"
  TALEA_BOOTSTRAP_TOKEN="$TALEA_BOOTSTRAP_TOKEN" TALEA_NAMESPACE="$NS" \
    python3 "$HERE/gen-workers.py" "$WORK" || die "could not render the worker manifests"
  log "rendered into $WORK"
}

# The in-cluster control plane talks to the API server, and the kubeconfig's
# certificate-authority-data is the server's leaf certificate rather than a CA,
# which Go refuses to verify. Inside the cluster that hop is already trusted, so
# the rendered copy skips verification instead of carrying a broken CA.
render_kubeconfig() {
  local src=$KUBECONFIG dst=$WORK/kubeconfig cluster
  cp "$src" "$dst" || die "no kubeconfig at $src"
  cluster=$(KUBECONFIG="$dst" kubectl config view -o jsonpath='{.clusters[0].name}')
  KUBECONFIG="$dst" kubectl config set-cluster "$cluster" --insecure-skip-tls-verify=true >/dev/null
  KUBECONFIG="$dst" kubectl config view --raw --flatten --minify -o yaml > "$dst.new"
  mv "$dst.new" "$dst"; chmod 600 "$dst"
  grep -q "insecure-skip-tls-verify: true" "$dst" || die "the rendered kubeconfig still verifies TLS"
}

do_config_objects() {
  render_kubeconfig
  kubectl -n "$NS" create configmap talea-config \
    --from-file=config.yaml="$WORK/config.yaml" --dry-run=client -o yaml |
    kubectl apply -f - || die "could not apply the config map"
  kubectl -n "$NS" create secret generic talea-kubeconfig \
    --from-file=kubeconfig="$WORK/kubeconfig" --dry-run=client -o yaml |
    kubectl apply -f - || die "could not apply the kubeconfig secret"
  log "config map and kubeconfig secret are in place"
}

# KnownFields(true) means one misspelled key stops the control plane from
# starting. Catch that here, in a Pod that exits, instead of in a crash loop.
do_check() {
  do_render
  do_config_objects
  kubectl -n "$NS" delete pod talea-config-check --ignore-not-found --wait=true >/dev/null 2>&1
  kubectl -n "$NS" apply -f - >/dev/null <<YAML || die "could not schedule the check Pod"
apiVersion: v1
kind: Pod
metadata:
  name: talea-config-check
  namespace: $NS
spec:
  restartPolicy: Never
  imagePullSecrets:
    - name: harbor-cred
  containers:
    - name: check
      image: $(grep -m1 'image:' "$HERE/40-control.yaml" | awk '{print $2}')
      imagePullPolicy: IfNotPresent
      command: ["/opt/tai-talea/bin/tai-talea"]
      args: ["--check-config", "--config", "/etc/tai-talea/config.yaml"]
      volumeMounts:
        - {name: config, mountPath: /etc/tai-talea/config.yaml, subPath: config.yaml, readOnly: true}
        - {name: kubeconfig, mountPath: /etc/tai-talea/kubeconfig, subPath: kubeconfig, readOnly: true}
  volumes:
    - name: config
      configMap: {name: talea-config}
    - name: kubeconfig
      secret: {secretName: talea-kubeconfig}
YAML
  local phase=""
  for _ in $(seq 1 40); do
    phase=$(kubectl -n "$NS" get pod talea-config-check -o jsonpath='{.status.phase}' 2>/dev/null)
    case "$phase" in Succeeded|Failed) break ;; esac
    sleep 3
  done
  kubectl -n "$NS" logs talea-config-check 2>&1
  kubectl -n "$NS" delete pod talea-config-check --wait=false >/dev/null 2>&1
  [ "$phase" = "Succeeded" ] || die "configuration check ended in phase $phase"
  log "configuration is valid"
}

do_core() {
  kubectl apply -f "$HERE/30-router.yaml" || die "router apply failed"
  kubectl apply -f "$HERE/40-control.yaml" || die "control apply failed"
  # The control plane registers workers through the router, so it must be able
  # to reach :30001 before any worker appears.
  for _ in $(seq 1 40); do
    local r c
    r=$(kubectl -n "$NS" get pod -l app=talea-router -o jsonpath='{.items[0].status.phase}' 2>/dev/null)
    c=$(kubectl -n "$NS" get pod -l app=talea-control -o jsonpath='{.items[0].status.phase}' 2>/dev/null)
    [ "$r" = "Running" ] && [ "$c" = "Running" ] && break
    sleep 6
  done
  kubectl -n "$NS" get pods -l 'app in (talea-router,talea-control)' -o wide
}

do_workers() {
  kubectl apply -f "$WORK/50-workers-a.yaml" || die "worker apply failed (a)"
  kubectl apply -f "$WORK/51-workers-b.yaml" || die "worker apply failed (b)"
  kubectl apply -f "$HERE/60-api.yaml" || die "api service apply failed"
  log "workers applied; the control plane adopts them from its pull snapshot"
  log "readiness is NOT pod readiness - use ops/taleactl.sh status"
}

case "${1:-all}" in
  secrets) do_secrets ;;
  render)  do_render ;;
  check)   do_check ;;
  core)    do_render; do_config_objects; do_core ;;
  workers) do_render; do_workers ;;
  all)     do_render; do_config_objects; do_core; do_workers ;;
  *) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
