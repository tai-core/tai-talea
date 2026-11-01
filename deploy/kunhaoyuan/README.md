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
- Images `talea-worker-0.9.2`, `talea-control-0.9.8`, and `glm-5.3-flash-ailog2`
  (the last is the official SGLang image with our Router binary appended as one
  extra layer; the Router runs from it, the control plane from its own tag).

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

## What looks fine and is not

**Pod readiness is not model readiness.** The worker's readiness probe is
`/bootstrap/health`, and that endpoint answers 200 with
`{"status":"ok","phase":"starting"}` while the weights are still loading. A
`4/4 Running` pool can serve nothing for another few minutes. Only a completion
over `/v1/chat/completions` proves the deployment is usable — that is what
`taleactl.sh up` and `verify.sh` wait for.

**One completion is not four workers.** The Router round-robins, so a single 200
says nothing about the other three, and `up` used to call itself done on exactly
that evidence. On 2026-11-01 that hid a pool running at 1/4 with everything
green; the surviving worker answered every probe while three were still loading
or had never been started at all. `ops/worker-registry.py` asks the narrower
question instead: for each Pod that exists right now, is its service URL both
registered as healthy *and* answering `/health`? `up` now fails unless every one
of them is.

Two of the Router's own answers cannot be used for this. `is_healthy` is a cached
health-check flag, so after a scale-down it keeps reporting `true` for workers
that are already gone — `down` finished with four Pods deleted and the registry
still claiming four ready. And the registry is keyed by URL, so an entry survives
the worker it described; counting healthy rows can be right about a generation
that no longer exists.

**A streamed request carries no tokens unless someone asks.**
`--enable-ai-access-log` writes the token fields from the upstream `usage`, and
OpenAI-compatible streaming omits `usage` unless the request says
`{"stream_options": {"include_usage": true}}`. Upstream the Router has no way to
force it, and a stream without it produces a log line with
`response_type: "stream"` and no `input_token`/`output_token`/`total_token` at
all — the one traffic shape a production gateway sees most of, unbillable.

So this deployment runs a Router built with one added flag,
`--force-stream-usage`: it rewrites streaming chat and completion requests to ask
for their usage, whether or not the client did. The rewrite is confined to the
two OpenAI routes; `/generate` reports usage in `meta_info` and has no
`stream_options` field to set. An explicit `include_usage: false` is overridden
rather than respected — the log is the billing record, and a client that opts out
makes the operator pay for traffic it cannot account for. `ops/check-ailog.py`
still counts token-less lines so the gap would reappear visibly if the flag were
ever dropped.

**A control plane that cannot apply the snapshot looks like one that agrees with
it.** Reading the partner's snapshot is the only stage of a pull round that
reported its own failures. Everything after it — applying an event, the
ownership check, the commit — returned its error to a caller that logged it at
debug and told nobody. That is not one lost round: the snapshot version stays
where it was, so the next round re-diffs the same baseline into the same events
and fails the same way, for as long as the cause lasts. A single instance the
store refuses to apply therefore freezes capacity reconciliation for good, and
the only trace is a `partner_sync_success_timestamp` that stopped advancing —
which reads as "nothing changed", exactly like health. Those failures are now
counted, logged and alerted through the same path as an unreachable partner,
from one deferred hook rather than at each return, so a stage added later cannot
be the one that forgets.

## Nightly start and stop

```bash
ops/taleactl.sh up        # scale the four workers to 1, wait until all of them serve
ops/taleactl.sh down      # close routing, let in-flight finish, scale to 0, prune
ops/taleactl.sh drain     # close routing only (the pool stays up, unused)
ops/taleactl.sh undrain   # put the workers back after a manual drain
ops/taleactl.sh prune     # drop registrations that no longer answer
ops/taleactl.sh status    # replicas, pods, cards requested, who is serving
ops/taleactl.sh hold 2    # suppress up/down for two hours (testing windows)
ops/taleactl.sh release
```

Crontab on the jump host:

```
0 20 * * * /bin/bash /root/taleactl.sh up   >> /var/log/taleactl.log 2>&1
0 8  * * * /bin/bash /root/taleactl.sh down >> /var/log/taleactl.log 2>&1
```

`/root/taleactl.sh` is a symlink into `ops/`, because the script locates its
helper scripts relative to itself and cron has to keep working.

The router and the control plane are deliberately left running: neither holds a
card, and a router restart throws away the worker ids it handed out. `kubectl
scale` is not granted to this account, so the script patches `spec.replicas`.

Measured on 2026-11-01: `down` 26 s; `up` 4 min 24 s from Pod creation to 4/4
serving, with all four workers loading together. The same measurement read
15 min 19 s while the loads were serial, and the load itself did not get faster —
a worker still takes 3 min 40 s (`service_starting` to `service_serving`). Both
ends of the comparison were run within the hour, so this run had a warm page
cache where the 15 min one was recorded cold; a cold one would add the same
delay to all four loads rather than multiply it.

Two later runs of the same script read 3 min 47 s (0.9.5), 7 min 24 s (0.9.6)
and 3 min 47 s (0.9.7). The 7-minute one is not a regression but the failure
mode described under known gaps: the prepare step finished 20 s apart for the
two pairs, and the planner round that saw only the first pair waited out their
whole load before launching the other two. Watch the `idle=` count in `planner
round applied` rather than the wall clock; a single wave is 3 min 45 s, and
anything longer means the instance set was split across rounds.

Starting the split on purpose — three workers, then the fourth 90 s later —
reads 5 min 14 s to full capacity under 0.9.7, because the late worker loads
underneath the first three instead of after them; the same sequence on the
waiting version would need about 7 min 30 s.

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

The wait is driven by each worker's own `sglang:num_running_reqs` (plus
`num_queue_reqs`), read from its Prometheus endpoint: that is the number of
requests the scheduler is generating for, and it is zero only when nothing is
generating. It has to be confirmed twice, two polls apart, because a request that
finished between polls and one that has not started yet look the same in a single
sample. `QUIESCE_TIMEOUT` (300 s) is a ceiling, not a delay — an idle pool drains
in about 10 s.

The first version of this used the Router's `/get_load`, which counts only what
is *queued* at a worker. Four concurrent generations read as zero load there, so
the wait degenerated into a fixed 20 s pause, and any generation longer than that
was still cut. Measured on 2026-11-01: 4096 tokens take about 16 s, which is
exactly why the earlier four-request test appeared to pass — the requests
happened to fit inside the pause. 8192 tokens do not.

`TALEA_SKIP_DRAIN=1` restores the previous scale-immediately behaviour.

### A revoked container is already gone

`down` scales the deployment, so the Pods disappear before the control plane has
finished draining them. The platform reports that as a revocation with a grace
period, which puts the instance in `DRAINING` with `pending_release` set — and
that state used to be a trap.

`reconcileInstance` dispatches a pending release straight to `reconcileReclaim`,
before the reachability probe that would mark an instance `LOST`. So the one
shortcut in `finishDrain` that skips `/bootstrap/stop` for a container that
cannot answer never fired for exactly the instances that needed it. Every round
then spent the full caller timeout on a dead address, and retried on the next
interval, forever: one immortal row per Pod generation, each one delaying the
work that was still real. On 2026-11-01 five generations of Pod names were
sitting in `DRAINING`, and a start took eight extra minutes because the
reconciler was queueing behind their timeouts.

The fix asks the partner before spending the stop budget. Absence from its lease
view is authoritative — it is the same answer `ReleaseInstance` already treats as
a completed release — so the drain collapses instead of retrying. An unreadable
lease view is *not* evidence of absence, so that case still stops the service as
before: skipping a stop that was needed leaves a process holding cards nobody is
accounting for.

Measured on the same day, four revoked workers: `service draining` at 03:23:30,
`service drain skipped the stop call` at 03:23:40, `instance released` at
03:23:40. Zero `context deadline exceeded`, zero `drain_timeout` alerts.

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

### Cache hits do show up, once there is a cache to hit

`input_token_details.cached_tokens` was recorded here as permanently absent,
with `--enable-cache-report` named as the missing piece. That was wrong: the flag
is in the worker arguments (`extra_sglang_args_by_role.regular`), and the field
appears as soon as a request actually hits the worker's radix cache. Measured on
2026-11-01 by sending the same 2546-token prefix twice:

```
request 1   usage={..., "prompt_tokens_details": null}
request 2   usage={..., "prompt_tokens_details": {"cached_tokens": 2496}}
```

The first request is a miss, and a miss is reported as `null`, not as zero. Every
test before this one had used a fresh prompt, so the conclusion "the feature is
off" came from a measurement that could not have shown anything else. Anyone
checking this later should repeat a prefix, not a request.

One caveat on the checker: the log is one append-only file, so a window exported
right after a Router upgrade still contains lines written by the previous binary.
A partial token triple there is history, not a regression — re-run once those
lines have aged out of the window.

## Left as known gaps

- A generation that outlives `QUIESCE_TIMEOUT` (300 s, roughly 75k tokens) is
  still cut by the Pod deletion, and the scale-down proceeds with a warning: at
  08:00 the cards have to come back. Raising the ceiling only moves the problem;
  the durable fix is on the worker side, where stop should refuse to return while
  `running_req > 0`.
- **A start no longer blocks anything it is not responsible for.** Reconcile, the
  pull sweep and LOST detection were freed in 0.9.5: a 0.9.6 cold start measured
  `operation is busy` = 0, `lease_expiring_soon` = 0, and every pull round applied
  all four of its events while the workers were loading.
- **A round no longer waits for the starts it dispatches.** Until 0.9.7 a planner
  round that had assigned two of four workers could not run again until those two
  finished loading, so a pool whose prepare step finished unevenly was started in
  two waves: 05:43:56 for two workers and 05:44:16 for the other two, `idle=2
  actions=2` in the round between them, second pair dispatched at 05:47:47 — 7 min
  24 s wall clock where one wave is 3 min 45 s. The concurrency bound moved from
  the round to the controller, so a round now returns after dispatch and leaves
  the instances it cannot serve to the next one.

  The 0.9.7 cold start came out as one wave — `idle=4 actions=4` at 06:09:52,
  last worker serving at 06:13:39, 3 min 47 s from dispatch, `up` 4 min 50 s end
  to end — so the split had to be induced to check the fix. Three workers were
  scaled up, and the fourth 90 s later. Round one saw three idle instances,
  `idle=3 actions=3` at 06:18:42; round two ran at 06:20:12, while all three
  starts it dispatched were still loading, and assigned the fourth
  (`serving=3 idle=1 actions=1` — `serving` here counts instances that hold a
  role, not instances whose service state is SERVING). The first three served at
  06:22:22-27, the fourth at 06:23:56 — 3 min 44 s after its own dispatch. Full
  capacity took 5 min 14 s where the waiting version would have needed about
  7 min 30 s, because it could not have run the 06:20:12 round at all. The split
  itself still happens: a round can only assign what is idle when it runs. What
  it no longer does is cost a second load window; it costs only the offset
  between the two rounds. `operation is busy`, `lease_expiring_soon`,
  `deferred instances` and `skipped an assignment` were all 0 across the run,
  which also means 0.9.6's store guard holds when a start lands three minutes
  after its siblings'.

  `TestARoundReturnsWhileItsStartIsStillLoading` keeps the unit-level reason:
  it parks a start in the gate and fails if the round does not return, which it
  does against the waiting version.

  The `lease_expiring_soon` warning this section used to attribute to the load
  had a different cause and is fixed in 0.9.6: the start persisted the whole row
  it had read before the load, which erased the observations the pull loop
  recorded meanwhile. The column itself was never stale — it went 05:36:23 ->
  05:32:53 -> 05:36:53 across the serving transition. An observation that moves
  backwards is now refused at the store.
- `--force-stream-usage` means a client cannot get a stream without the trailing
  usage chunk. That is deliberate, but it is a behaviour a client could notice.
- The control plane's instance store is an `emptyDir`, so rolling `talea-control`
  empties it and the next pull has to re-adopt every Pod. That costs a re-adoption
  round, not a reload — `/bootstrap/start` against a worker that is already
  serving answers "already running" and the control plane adopts it, which is what
  the 03:21 re-adoption did in four seconds.
- **The public entry is not reachable from the internet yet**, and the fix is not
  on this host. nginx on the jump host listens on `0.0.0.0:80` and `:8000`, has no
  local firewall, and answers 200 to `127.0.0.1:8000/health`. But the host's only
  address is `10.5.0.2`; `36.138.20.89` is a platform EIP, and that EIP answers
  `502 upstream connect failed: connection timed out` in the platform proxy's own
  `text/plain` format — nothing reaches nginx. The EIP has to be mapped to
  `10.5.0.2:80` and the port opened in the console's security group; until then
  the deployment is only reachable as `192.168.239.{2,3,4}:31280` from inside.
