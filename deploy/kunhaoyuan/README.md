# kunhaoyuan: four B300 nodes' worth of GLM-5.3-Flash, aggregated

The deployment that has been running on the 中国移动 B300 cluster since 2026-11-01.
Two nodes, four workers of four cards each, one aggregated `regular` role — no
prefill/decode split. The sibling `deploy/kubernetes/` directory holds the
nightshift (H100) shape; it uses hostNetwork and a hand-placed Router, which this
namespace cannot do.

```
client ── NodePort 31280 ──> talea-router (8000 serving, 30001 control)
                                 ▲   │ PUT /state/workers/{id}
                      pull+register│   ▼
                            talea-control (8787 admin)
                                 ▲   │ lists Pods by label
                                 │   ▼
              talea-w0..w3 (Deployment, 4×nvidia.com/gpu, role=regular)
                                 │
                                 └── /logs, /root/.cache on /data02 (node-local)
```

## What has to exist first

- Namespace `kunhaoyuan`, and `harbor-cred` in it:
  ```
  kubectl -n kunhaoyuan create secret docker-registry harbor-cred \
    --docker-server=<registry> --docker-username=<user> --docker-password=<pass>
  ```
- Weights at `/data02/model-weight/GLM-5.3-Flash-NVFP4` on both nodes, and a
  writable `/data02` for the compile cache and the token log.
- Images `talea-worker-0.9.2` and `talea-control-0.9.2` in the registry above.

## Deploy

```bash
./apply.sh secrets     # once: writes /root/.talea-secrets.env with three random tokens
./apply.sh check       # renders, applies the ConfigMap, runs tai-talea --check-config
./apply.sh all         # router, control, workers, NodePort service
./verify.sh            # real completion + token log conformance
```

`apply.sh` renders `config.yaml.tmpl` and the worker manifests into a private
directory. The four shared secrets (`admin_token`, `bootstrap_token`,
`push_token`, `push_secret`) live only in `/root/.talea-secrets.env`: this
repository is public, and the bootstrap token also has to appear literally in the
worker probe headers, where Kubernetes offers no secret reference. Re-rendering
reads the same file, so a re-apply does not by itself restart anything.

## Two things that look fine and are not

**Pod readiness is not model readiness.** The worker's readiness probe is
`/bootstrap/health`, and that endpoint answers 200 with
`{"status":"ok","phase":"starting"}` while the weights are still loading. A
`4/4 Running` pool can serve nothing for another few minutes. Only a completion
over `/v1/chat/completions` proves the deployment is usable — that is what
`taleactl.sh up` and `verify.sh` wait for.

**A streamed request carries no tokens unless the caller asks.**
`--enable-ai-access-log` writes the token fields from the upstream `usage`, and
OpenAI-compatible streaming omits `usage` by default. Without
`{"stream_options": {"include_usage": true}}` in the request the log line has
`response_type: "stream"` and no `input_token`/`output_token`/`total_token` at
all. The Router has no flag to force it; a production gateway has to inject it.
`ops/check-ailog.py` counts these so the gap stays visible.

## Nightly start and stop

```bash
ops/taleactl.sh up        # scale the four workers to 1, wait for a real completion
ops/taleactl.sh down      # close routing, let in-flight finish, then scale to 0
ops/taleactl.sh drain     # close routing only (the pool stays up, unused)
ops/taleactl.sh undrain   # put the workers back after a manual drain
ops/taleactl.sh status    # replicas, pods, cards requested, endpoint
ops/taleactl.sh hold 2    # suppress up/down for two hours (testing windows)
ops/taleactl.sh release
```

Crontab on the jump host:

```
0 20 * * * /bin/bash /root/taleactl.sh up   >> /var/log/taleactl.log 2>&1
0 8  * * * /bin/bash /root/taleactl.sh down >> /var/log/taleactl.log 2>&1
```

The router and the control plane are deliberately left running: neither holds a
card, and a router restart throws away the worker ids it handed out. `kubectl
scale` is not granted to this account, so the script patches `spec.replicas`.

Measured on 2026-11-01: up 263–304 s (warm page cache), down 23–32 s.

### What `down` does before it scales

`down` first sets each routable worker to `unavailable` in the Router
(`PUT /state/workers/{id}`) and waits, so the requests that were already running
finish instead of being cut off by a deleted Pod. Requests that arrive during
that window get an honest 503 — the pool is going away — rather than a
connection to a process that is stopping.

Two details that cost real debugging:

- The control plane's own `POST .../drain` is *not* this. It waits for load and
  then stops SGLang, but it only closes Router readiness when the instance has no
  recorded worker id, so a serving worker keeps receiving traffic right up to the
  moment it dies.
- `reason` on that endpoint is a closed enum (`manual` | `health_check`); a
  free-text explanation is answered with 422.

The wait is a settle window (20 s) plus a load probe, because `/get_load` counts
what is queued at the worker, not what is generating: four concurrent requests
were observed reading as zero load. A generation longer than the window can
still be cut when the Pod is deleted. `TALEA_SKIP_DRAIN=1` restores the old
scale-immediately behaviour.

## Token log

`ops/export-ailog.sh [lines]` copies `/data02/talea-ai/logs/talea-router.log`
off the node (the account has neither `pods/exec` nor `port-forward`, so a
read-only hostPath Pod is the only way to read node-local disk), extracts the
`{"ai_log":...}` lines and runs `ops/check-ailog.py` over them: the two-layer
contract, unique `request_id`, millisecond durations, zoned `start_time`, nested
token details.

The file is a mirror of stdout, so it is mostly tracing noise at ~75 MB/day;
token lines are a few hundred bytes per request. The Router already emits the
nightgw-compatible shape (`gateway_type: "nightgw"`, `nightgw-` request ids), so
no translation layer is needed before the log reaches Vector.

## Left as known gaps

- A generation longer than the 20 s settle window can still be cut by the Pod
  deletion; the fix belongs on the worker side (stop waiting for
  `running_req == 0`), not in the scale-down script.
- `input_token_details.cached_tokens` never appears: the worker reports
  `prompt_tokens_details: null`. Enabling `--enable-cache-report` on the workers
  would populate it and let the cache-hit statistic leave zero.
- The Router's worker registry keeps an entry per registration, not per address,
  so a scaled-down pool leaves entries behind. They show as unhealthy and are
  skipped by the drain script, but the list grows across restarts.
