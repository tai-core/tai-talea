# nightshift 恢复：用 Router 复位解开角色变更死结（2026-10-14）

> 承接 `nightshift-outage-and-host-contention-2026-10-14.md`（停摆与宿主争用）。
> 本篇只讲**恢复**：清场 → 救活僵尸容器 → 遇到并解开「角色变更死结」→ 拿回 1P1D。
> 时间一律 **UTC**（CST = UTC+8）。

---

## 0. 结论速览

| # | 项 | 结果 |
| --- | --- | --- |
| 1 | 最终服务状态 | **1P1D 成立**：Router `prefill_count=1 decode_count=1`，`healthy_workers=2`，`/readiness` **200**，经 Router 推理 **200** |
| 2 | 拓扑 | `.70`(h100-04) = **prefill**；`.67`(h100-01) = **decode**；两者均 `state=ready`、无 `unavailable_reason` |
| 3 | 关键阻碍 | 控制面 `.70` 的角色变更（prefill→decode）被卡死 **20 分钟**，`IDLE/REGISTERING ↔ IDLE/FAILED` 每 ~14s 循环 |
| 4 | 卡死根因 | **两层叠加**：① `register()` 用 `call_timeout: 10s` 包住本该 390s 的 Router 删除工作流；② 要删的陈旧条目是 `type=prefill`，撞上 `drain_pd_workers` 的 `Blocked::AmbiguousPrefillTransfer` 闩锁（**只作用于 Prefill**） |
| 5 | 解法 | **原地重启 Router 容器**（杀容器 PID 1），清空进程内成员表 / 闩锁 / 旧 `manual` 摘流记录 |
| 6 | 为什么有效 | 重启后 `.70` 以 sglang 的**真实身份 `decode`** 重新注册 ⇒ planner 要做的转换变成 **decode→prefill** ⇒ 需要删的是 **decode** 条目 ⇒ **绕开歧义闩锁** |
| 7 | 代价 | Router 成员表清空、**worker id 全换**（`.67`→`6706184e…`、`.70`→`2f073b45…` 再到 `d4045cba…`）；`worker_removal` 作业队列丢弃。服务无感知 |
| 8 | 墙钟 | 15:10:24 死结解开 → 15:13:49 prefill `SERVING` → 15:14:25 验收 200（**约 4 分钟**） |

---

## 1. 恢复前的状态（15:09 左右）

清场与僵尸复活之后，两台 sglang **都以 `decode` 角色**跑着（`.70` 的容器被原地重启过，`.67` 是刚起来的）。此时：

```
talea-cmcc-k8s-1010-172229     IDLE  svc=REGISTERING  role=decode  a=2
talea-cmcc-k8s-1010-172229-1   IDLE  svc=SERVING      role=decode  a=0

router /workers      prefill_count=1 decode_count=1
                     http://10.214.81.70:9002  type=prefill  healthy=True  ← 陈旧条目
                     http://10.214.81.67:9002  type=decode   healthy=True
router /readiness    503 insufficient healthy workers
```

`.70` 的 `last_error` 固定不变：

```
router registration failed during reconcile: router accepted the removal of
http://10.214.81.70:9002 but still lists it as "prefill" after 10s
```

且 `reconcile instance failed` 每 ~14 秒重复一次，**错误逐字相同** —— 这是「结构性卡死」而不是「偶发失败」的信号。

---

## 2. 死结的两层机制

### 2.1 A 层：10 秒天花板（`call_timeout` 包住了整段等待）

`register()`（`internal/controller/lifecycle.go:418`）在重新注册前要清理陈旧成员，这一步调用 `RouterAdapter.RemoveStaleMembership`，而整段等待被 `c.callContext(ctx)` 包住：

- `Config.Timeout`（`routeradapter` 的 `Timeout`）只约束**单次 HTTP 请求**；
- **整段预算在 ctx deadline**，而 `call_timeout: 10s`（线上配置第 41 行）。

Router 的删除是**异步工作流**（`worker_removal`），预算最多 **390s**。⇒ 控制面**在结构上不可能**观察到删除落地，10 秒必超时。

> 同源教训（上一轮）：`RemoveStaleMembership` 超时后原样抛出 `context deadline exceeded`，让人误以为是端点慢——实测 `GET /workers/{id}` 是 **0.00s**。

### 2.2 B 层：歧义闩锁（决定性）

`drain_pd_workers.rs:185-284` 的阻塞判定中，**歧义检查只作用于 `Prefill` 类型**：

```rust
// Blocked::AmbiguousPrefillTransfer
if worker.worker_type == Prefill && worker.has_uncertain_prefill_transfer() { ... }
```

`Decode` 只看 `load == 0`。

`.70` 的陈旧条目恰好是 **`type=prefill`**，且它正是**池里唯一的 prefill** ⇒ 歧义闩锁必然生效。我们绕过控制面**直接**在 Router 上发 `DELETE /workers/{id}` 并轮询 **390 秒**，条目依旧在：

```
Failed job: type=RemoveWorker
PD drain budget expired; keeping the worker
```

⇒ **B 层单独就足以否决「删掉那个 prefill 条目」**，A 层的 10 秒只是让它看起来像超时。

### 2.3 附带事实：控制面刻意不调 `DELETE`

`drain_pd_workers` **只在 Router 的 `worker_removal` 工作流里跑**，而该工作流**只由 `DELETE /workers/{id}` 启动**；控制面常规路径从不发这个 DELETE（`acceptance_test.go` 有断言）。

⇒ 结论：**删人的活儿只发生在 `register()` 的陈旧成员清理里**。这也是为什么这个 bug 平时碰不到、一旦碰到就永久卡死。

---

## 3. 为什么「重启 Router」是正解而不是权宜

Router 的 **worker 池 / `/state/workers` readiness / 歧义闩锁 / 作业队列全在内存**（刻意设计，见 `worker_registry.rs`）。重启清空：

1. 陈旧 `type=prefill` 成员条目（连同它带的 `manual` 摘流记录——`url_to_id` 是进程内 DashMap，重启后 id 全换，摘流随之丢失）；
2. `has_uncertain_prefill_transfer()` 闩锁。

之后控制面按 **sglang 自己的真实身份**（两台都跑着 `--disaggregation-mode=decode`）重新注册，于是：

| 阶段 | total | planner 目标 | 动作 |
| --- | --- | --- | --- |
| 重启后两台都注册为 decode | 2 | 1/1 | **恰好一次** decode→prefill |

⇒ 这一次要删的是 **decode** 条目，**不触发歧义闩锁**。这就是重启能解开的全部原因。

> planner 计数口径（`planner.go:649-676`）：`total = len(Serving)+len(Idle)`，`TargetPrefill = floor(total*ratio)`，**只有 `total>=2` 才夹到 `[1,total-1]`**，`prefillPlanned/decodePlanned` 按 **Serving** 计 ⇒ **只会转换一次，不会连环**。

---

## 4. 操作

### 4.1 为什么是「杀容器 PID 1」而不是「删 Pod」

`talea-router` 是 **无 owner 的裸 Pod** + `restartPolicy=Always`：

- 删 Pod ⇒ **容器消失**，没有任何控制器把它拉起来；
- 杀容器 **PID 1** ⇒ kubelet 看 `restartPolicy=Always`，**原地重启同一个容器**：同 Pod 名、同 hostNetwork 地址、同租约。

（同一招已在 worker 容器上验证过：`restartCount=1`、`lastState.terminated.exitCode=137`，Pod 名与 `10.214.81.70` 不变。）

### 4.2 两个必须的守卫

**① 探针自匹配**：探针脚本自己的命令行里就含 `sglang-router` 字面量，`case "$cmd" in *sglang-router*)` 会把探针自己和两个子 shell 一起匹配上（本轮前一次已经在 worker 上踩过同样一坑，`entrypoint.sh … serve` 命中 3 个）。修法：按**二进制名前缀**整段比对：

```bash
case "$cmd" in
  "/usr/local/bin/sglang-router "*) echo "$p|..." ;;
esac
```

并要求**恰好一个**候选落在 `/kubepods/...` 里，否则 `REFUSING`。

**② 存活判定不能用 `127.0.0.1`**：探针 Pod 有**自己的 netns**（没有 `hostNetwork`），`ss -lntp | grep :8000` 与 `curl 127.0.0.1:8000` 都看不到 hostNetwork 上的 Router。只有 Pod 内**不可能**判断，必须打 hostNetwork 地址：

```bash
curl -s -m 5 -o /dev/null -w "%{http_code}" http://10.214.81.70:8000/health
```

**③ HTTP 一律在 Pod 里发**：`n1`（跳板）**到不了** `10.222.x`（控制面）与 `10.214.x`（Router）。所有探针走 `nsrun.py <driver> --ctrl-pod tai-talea`，控制面地址由 `kubectl get pod -o jsonpath` **动态解析**（控制面没有 Service，每次滚动换集群 IP）。

### 4.3 实测输出

```
### Router processes (prefix match on the binary)
  pid=306712 ... cmd=/usr/local/bin/sglang-router --host=0.0.0.0 --port=8000 --pd-disaggregation ...
  health before: 200
### killing pid 306712 (Router container init)
  sent
  +10s health=200  old-pid-alive=no  router-procs=10
### after restart
  313537|.../podd15a4afd-.../...|/usr/local/bin/sglang-router --host=0.0.0.0 --port=8000 --pd-disaggregation \
     --prefill-policy=session_aware --decode-policy=round_robin \
     --health-check-interval-secs=5 --health-check-timeout-secs=3 --health-success-threshold=1
     health   -> 200
     /workers -> {"workers":[{"id":"2f073b45-...","url":"http://10.214.81.70:9002","worker_type":"decode",...}]}
```

⚠️ **重启必须带健康节奏参数**：`--health-check-interval-secs=5 --health-check-timeout-secs=3 --health-success-threshold=1`。默认 60s×2 成功 ⇒ 新 worker 最坏 120s 才判 healthy，而控制面 `ensureReadiness` 只等 `6×5s=25s` ⇒ **已成功的 start 被稳定判死**，烧完 `start_attempts` 永久停摆。

---

## 5. 验收证据（控制面日志逐条）

```
15:10:24  service membership reconciled  172229  role=decode  router_worker_id=2f073b45-…   ← 死结解开
15:10:28  service membership reconciled  172229-1 role=decode router_worker_id=6706184e-…
15:10:28  planner round applied  summary="gear=1:1 target=1/1 serving=2 idle=0 actions=1"
15:10:28  service draining  172229  reason="planner rebalance decode -> prefill"  grace=5m0s
15:10:36  service drained   172229  forced=false  exit_code=68          ← 优雅 drain，仅 8 秒
15:11:32  dispatching start to the bootstrap  172229  role=prefill
15:13:49  service serving   172229  role=prefill  router_worker_id=d4045cba-…
```

终态（15:14 与 15:16 两次快照一致）：

```
talea-cmcc-k8s-1010-172229     IDLE  svc=SERVING  role=prefill  a=0  gen=11
talea-cmcc-k8s-1010-172229-1   IDLE  svc=SERVING  role=decode   a=0  gen=5

router /state/workers   .67 state=ready reason=-   .70 state=ready reason=-     ← 摘流记录已清
router /workers         prefill_count=1 decode_count=1
                        .67 type=decode  healthy=True
                        .70 type=prefill healthy=True   id=d4045cba-…
router /readiness       200 {"healthy_workers":2,"available_workers":2,"total_workers":2}
inference               200 ×2（两轮共 4 次全 200）
```

**告警自动消失**：

| 告警 | 最后一条 | 说明 |
| --- | --- | --- |
| `service_start_failed` (warning) | 15:09:58 | 重启前最后一条，之后再无 |
| `lease_expiring_soon` (warning) | 15:13:51 | 判据是 `now - LeaseUpdatedAt >= lease_warning(120s)`；两台行的 `lease_updated_at` 现在每轮 pull 都刷新（15:16:00Z），定时消失 |

**GPU 余量**（清场后仅剩 `glm-chy` / `eval-chy`，占用 0）：

| 节点 | 每卡占用 | 每卡余量 | 外来容器 |
| --- | --- | --- | --- |
| h100-04 | 70.5–71.0 GiB / 81.5 GiB | **≈ 10.5 GiB** | 无（仅两个 0 占用容器） |
| h100-01 | 71.0–71.3 GiB / 81.5 GiB | **≈ 10.4 GiB** | 无 |

> 注：`mem-fraction-static=0.85` 在 CUDA graph 捕获阶段仍偏紧——上一轮 decode 的 `-9` 正是捕获期 `torch.OutOfMemoryError`（某次只剩 **16.69 MiB**）触发 sigquit → `kill_process_tree`。第三方用 **0.70**。本轮 prefill 顺利通过，建议下一轮部署时把该旋钮下调。

---

## 6. 遗留修复项（按优先级）

1. **`register()` 的陈旧成员清理必须用长于 Router 工作流预算的 ctx**（现被 `call_timeout: 10s` 封顶），并且超时时**原样返回传输层错误**这件事要改——错误文案应说明「等待预算不足，Router 侧可能仍在处理」。
2. **歧义闩锁不该闩住「没有转移状态」的 worker**：`has_uncertain_prefill_transfer()` 目前是黏性闩锁，一个从未参与过 transfer 的 prefill 也被永久挡住。更稳的方向是**控制面驱动「先加后删」**（新身份先注册成功，再删旧条目），从根上避免「删掉唯一 prefill」这种形状。
3. **把「Router 内存态陈旧」当一等故障模式**：现在只靠人工判断 + 重启。至少要有一个自检（例如：控制面已 `SERVING` 但 Router 里同一 URL 的类型/健康态长期不一致 ⇒ 抛明确告警，而不是每 14 秒重复同一条 `reconcile failed`）。
4. **planner 下限保护尚未部署到线上**（本地已有：某角色只剩 1 个时禁止把它 rebalance 成另一种角色，`TestPlanNeverEmptiesARole`）。本轮 `172229` 的转换发生在 `serving=2` 时，所以没触发；但线上跑的仍是**未部署该保护的旧版**。
5. **给 worker Pod 加 livenessProbe**：现在只有 readiness（9001 `/bootstrap/health`），僵住的进程树 k8s 永不重启（实测僵了 10 小时）。

---

## 7. 复用清单（`.workbuddy/tmp/alayanew/`）

| 脚本 | 作用 |
| --- | --- |
| `nsrun.py <driver> --ctrl-pod tai-talea [--host] [--hostpid]` | 在集群内/节点上跑探针；自动解析控制面 Pod IP |
| `ns-now.py` | 一次给全：实例行 + `/state/workers` + `/workers` + readiness + 推理（`--host` 时附带线上显存相关 flag） |
| `ns-recovery.py` | 同上但含两轮推理 |
| `ns-await-pd.py` | 等到 `readiness==200` 自动打两枪；判据 `READY / BURNED_OUT / TIMEOUT` |
| `ns-revive-router.py` | 安全重启 Router 容器（二进制名前缀匹配 + `/kubepods` 唯一性守卫） |
| `ns-revive-decode.py` | 安全重启 worker 容器（整行精确比对 `bash /opt/tai-talea/entrypoint.sh serve`） |
| `ns-gpu-only.py` | 外来容器 + 每卡占用 + 按 cgroup 归属（需 `--host --hostpid`） |
| `ns-lease.py` | lease 告警取证（配置旋钮 + 每行 `lease_updated_at`） |
