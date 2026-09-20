# talea-worker Helm Chart

This chart runs the container-local Talea bootstrap on an NVIDIA Kubernetes
worker. Talea assigns the `prefill` or `decode` role and starts SGLang through
the authenticated bootstrap API. Leave `worker.role` empty for normal control
plane ownership.

```bash
kubectl -n inference create secret generic talea-worker-auth \
  --from-literal=token='<CONTROL_PLANE_BOOTSTRAP_TOKEN>'
helm upgrade --install worker-p . \
  --set image.repository=<partner-registry>/talea-worker \
  --set image.tag=<immutable-tag> \
  --set bootstrap.tokenSecretName=talea-worker-auth \
  --set cache.existingClaim=<model-pvc>
```

For a fixed smoke test only, add `--set worker.role=prefill` or `decode`.
Do not commit a real token. Readiness checks the authenticated bootstrap and
does not mean that the P/D service is already serving traffic.
