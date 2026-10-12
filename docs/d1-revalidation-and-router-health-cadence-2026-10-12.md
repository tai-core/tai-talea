# D1 复验 + Router 健康探测节奏（2026-10-12）

> 平台：nightshift k8s（中国移动 ecis），ns `nightshift`，节点 h100-01 / h100-04（各 8×H100）
> 结论：**1P1D 恢复 SERVING，经 Router 真实推理 200**。
> 前置：控制面 v0.4（`library/tai-talea:v0.4`，digest `sha256:fdfeeb2e…`），
> 本轮**未改控制面镜像**——先把"为什么起不来"查清，再决定改什么。

## 1. 故障现象与它骗人的地方

两个 worker 长期停在 `IDLE`，`service_state` 在 `FAILED / STARTING` 之间循环。
控制面每轮报同一句错：

```
reconcile instance failed  instance_id=talea-cmcc-k8s-1010-172229
  error="router readiness failed: router worker is not routable: healthy=false pd_state="
```

以及后来：

```
error="router registration failed during reconcile:
       Get \"http://10.214.81.70:8000/workers/f3121067-…\": context deadline exceeded"
```

**第二句是有害的**：它点名一个端点加一个传输层超时，读起来像"Router 这个接口很慢"。
实测 `GET /workers/{id}` 延迟 **0.00s**。按这句话去查，会一直查错方向
（本轮为此花掉了大半时间）。

## 2. 真正的三层原因

### 2.1 控制面的就绪窗口比 Router 的健康收敛更快（主因）

Router 的健康检查是**周期性**的，而它的节奏是启动参数，不是控制面能看见的东西：

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `--health-check-interval-secs` | **60** | 多久探一次 |
| `--health-success-threshold` | **2** | 连续成功几次才判 healthy |

⇒ 一个刚起来、`/health` 已经 200 的 worker，最坏要 **~120 秒**才会被 Router 判为 healthy。

而控制面 `lifecycle.go:ensureReadiness` 的重试预算只有
`6 次 × 5 秒 = 25 秒`。于是**一个实际已经成功的 start 被稳定判死**：

- `failStart` 烧掉一次 `start_attempts`，
- `reconcile.go:224` 在 `StartAttempts >= StartMaxAttempts(3)` 时 `return nil` 放弃重试，
- 而 `rebalance.go` 对 `FAILED` 实例**完全不可见**
  （`hasLiveService(FAILED)=false` ⇒ 不算 serving；`Role.Valid()` 为真 ⇒ 不算 idle），

两条路同时关闭 ⇒ 实例永久停摆。这正是 13:56 那次 requeue 之前的状态。

时间线可核对：13:56:26 下发 start → 13:59:32 报 readiness 失败，
差值 3m06s ≈ 模型加载 ~2m40s + 6 次探测的 25s 窗口。而 Router 侧
`is_healthy` 在 14:02 已经是 `true`。

**修复**：给 Router Pod 加参数让它按控制面的时间尺度工作
（`deploy/kubernetes/talea-router.yaml`）：

```
--health-check-interval-secs=5
--health-check-timeout-secs=3
--health-success-threshold=1
```

收敛变成 ~5 秒，是 25 秒窗口的 1/5。`failure-threshold` 保持 3，避免慢探测把好 worker 抖出去。

### 2.2 Router 的角色记录比进程活得久（恢复路径的活锁）

Router **没有"原地改 worker 角色"的接口**——`PUT /workers/{id}`（`update_worker`）
只改 labels / priority / cost / health，`worker_type` 取的是**旧值**
（`update_worker_properties.rs:95` `.worker_type(worker.worker_type().clone())`）。
所以角色变更只能是 **delete + add**。

而 `finishDrain`（`lifecycle.go:899`）清掉角色的同时**不动 Router 成员**，
于是 drain 过的 URL 在 Router 里保留旧角色。下次以新角色注册时走
`register()` 的 stale-role 恢复路径：`SetReadiness(false)` → `RemoveStaleMembership` → 重新注册。

`RemoveStaleMembership`（`routeradapter/client.go:399`）**本身就在等删除生效**
（轮询 `GET /workers/{id}` 直到 404）。而 Router 的删除是**异步工作流**
（`worker_removal`：`find_workers_to_remove` → `drain_pd_workers` → …），
所以 DELETE 返回 202 时条目还在。它用 **100ms 定频**轮询，跑满 10s 的 `call_timeout`：

- 日志侧：`/workers/{id}` 被打到 ~10 req/s 持续数分钟（Router 日志被刷屏）；
- 报错侧：预算耗尽时 `GetWorker` 抛出裸的
  `Get "http://…/workers/f3121067-…": context deadline exceeded`，
  **被原样返回给调用方** ⇒ 就有了 §1 里那句骗人的错误。

> 这段设计是**有意**的，`client.go` 头部契约写明
> "Draining is expressed exclusively through readiness. DELETE is limited to replacing
> an unavailable, idle membership whose role is stale (§8 recovery)"。
> 因此本轮**没有**改成"drain 时删成员"——那是改契约。
> 而且安全性靠 `state=unavailable/manual` 兜着：drain 后 readiness 是 false，
> 即使新进程起来把 `is_healthy` 翻成 true，Router 也不会路由过去
> （`ensureReadiness` 才会 `SetReadiness(true)`）。

### 2.3 起步：Router 的内存态已经被前面积累搞坏了

`drain_pd_workers` 这个 step 的超时是 **`Duration::MAX`**（`local/mod.rs:229`），
`wait_until_idle`（`drain_pd_workers.rs:16`）在 worker 非 idle 时**静默**死等。
Router 的 worker 池 / readiness / 作业队列**全在内存**（模板注释：这是刻意的，
"a restarted Router comes up empty and waits to be told again"）。

这台 Router 已经跑了 **44h**，期间积累了 `__pd_state=draining` 且永不消失的成员，
DELETE 每次都返回 202、每次都不落地。**重启 Router 后同样的 DELETE 立刻成功**
（新注册拿到全新 worker id）——所以这是**积累态**问题，不是删除逻辑本身坏了。

## 3. 本轮改了什么

| 文件 | 改动 |
| --- | --- |
| `deploy/kubernetes/talea-router.yaml` | 三个健康检查参数 + 一段说明"这个节奏控制面在等" |
| `internal/routeradapter/client.go` | `RemoveStaleMembership` 的可见性轮询改退避（250ms→2s 封顶）；预算耗尽时改报"Router 接受了删除但 Xs 后仍列出它，角色是 Y，陈旧成员没被摘掉"，并**实测**等待时长而不是复述 `config.Timeout` |
| `internal/controller/lifecycle.go` | `waitMembership` 同样退避；新增 `membershipError`，把裸的传输超时换成"角色 + Router 实际报的类型 + 预算" |

新增/更新的测试：

| 测试 | 覆盖 |
| --- | --- |
| `routeradapter::TestRemoveStaleMembershipWaitsWithoutHammeringTheRouter` | 删除延迟落地时能等到；轮询次数 ≤ 8（定频 100ms 会花 30 次） |
| `routeradapter::TestRemoveStaleMembershipNamesAMembershipThatNeverDrops` | 永不落地时报错必须含端点、旧角色、"was not dropped"；且确实等满预算 |
| `controller::TestMembershipWaitBacksOffAndNamesTheRoleItNeverSaw` | 角色永远不匹配时，报错含请求的角色与 Router 实报的类型，轮询 ≤ 10 |
| `routeradapter` 测试替身 | 新增 `removalDelay` / `pendingDrop` / `gets`，把"接受删除但延迟消失"建模进去 |

> 写第 3 个测试时踩到一个真问题并修掉了：`Config.Timeout` 只约束**单次 HTTP 请求**，
> 真正的整段预算在 **ctx 的 deadline** 上（控制面走 `callContext`）。
> 测试里用 `context.Background()` 会让 `RemoveStaleMembership` **永远转下去**——
> 测试挂死正是这么来的。这也说明该函数的预算完全由调用方决定，文档要写明。

Go 全量 17 包 `ok`。

## 4. 证据：D1 复验

### 4.1 控制面与 Router

```
talea-cmcc-k8s-1010-172229    IDLE  svc=SERVING  role=prefill  a=0  worker=0f953c25
talea-cmcc-k8s-1010-172229-1  IDLE  svc=SERVING  role=decode   a=0  worker=038e3175

GET /workers   stats={'prefill_count': 1, 'decode_count': 1, 'regular_count': 0, 'pending_count': 0}
  http://10.214.81.70:9002  type=prefill  is_healthy=true  load=0
  http://10.214.81.67:9002  type=decode   is_healthy=true  load=0
GET /readiness -> 200 {"status":"ready","healthy_workers":2,"available_workers":2,"total_workers":2}
GET /state/workers -> 两条都 state=ready（无残留的 manual 摘流）
```

### 4.2 推理（经 Router，非直连 worker）

```
--- arithmetic -> 200 in 0.47s   finish=length completion_tokens=16
    17 * 23 = 391  Let me verify: 17 *
--- chinese    -> 200 in 1.03s   finish=length completion_tokens=64
    … PD分离 refers to "Prefill-Decode Disaggregation" …
--- longform   -> 200 in 1.96s   finish=length completion_tokens=160
    … Prefill phase: … compute-bound … Decode phase: generates one token at a t…
### verdict: 3/3 completions returned real, metered output
```

`usage.completion_tokens` 有值 ⇒ 不是空壳 200。算术题答案正确（391）。

### 4.3 planner 自己走完了 decode→prefill 的转换

这是本轮最值得记的一段——**没有人工干预角色**，全部由 planner 驱动：

```
14:13:06  service membership reconciled  -1      role=decode  worker=038e3175
14:13:09  service membership reconciled  172229  role=decode  worker=cc61fddd
14:13:12  service draining               172229  reason="planner rebalance decode -> prefill" grace=5m
14:13:12  planner round applied  summary="gear=1:1 target=1/1 serving=2 idle=0 actions=1"
14:13:24  service drained                172229  forced=false exit_code=68      ← 12 秒
14:14:18  dispatching start              172229  role=prefill
          --disaggregation-ib-device=mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_5,mlx5_6,mlx5_7,mlx5_8
14:17:45  service serving                172229  role=prefill worker_id=0f953c25  ← 全新注册
```

注意 14:14:18 那一行：**角色从 decode 换成了 prefill，`--disaggregation-ib-device`
仍是 h100-04 自己的名单**（含 `mlx5_5,mlx5_6,mlx5_7,mlx5_8`，不含 `mlx5_4` 那张 200Gb HDR）。
这正是 `docs/role-and-node-argument-overrides-2026-10-10.md` §4.5 要的性质：
参数跟机器走，不跟角色走。

稳态从 14:18 到 15:23（**65 分钟**）无抖动，`pd_ratio_drift` 告警消失。

## 5. 遗留

### 5.1 Router fork：`drain_pd_workers` 可以永久阻塞

`local/mod.rs:229` 把这一步的超时设成 `Duration::MAX`，
`wait_until_idle` 在 `load != 0` 或 `has_uncertain_prefill_transfer()` 时**静默**死等
（后者只打一次 `error!`，前者一句都不打）。
后果：一个 worker 的删除工作流可以永远占着，它的 `__pd_state` 永远是 `draining`。

**这是我们的 fork，应该修**：给这一步一个有限超时，并把"还在等"这一状态
暴露到 `/workers/{id}` 的 `job_status` 里（现在只有 `pending`）。
本轮**未修**——本机没有 Rust 工具链，改完无法编译验证，属于另一件事。

### 5.2 控制面：一个 READY 的 Router 也会撞同一面墙

§2.2 的 stale-role 恢复路径在**正常运行的 Router** 上也会走一次
（drain 留下的旧角色），最快也要一个额外 `call_timeout`。
本轮因为重启了 Router（成员表是空的）所以没撞上。

### 5.3 启动预算的分配

`StartService` 在**开头**就 `StartAttempts++`，所以任何一次重试——
包括"只是 Router 还没把角色换过来"这种几秒钟的瞬时条件——都要花掉 3 次预算里的 1 次。
配合 `reconcile.go:224`，3 次烧完就彻底停摆。
`RequeueService`（本轮之前加的 `POST /v1/capacity/instances/{id}/requeue`）是**手工**出口；
要不要给"基础设施瞬时错误"单独一个不烧预算的重试通道，是个待定的设计问题。

### 5.4 D2 未做

`nightshift-automation-gap-2026-10-09.md` §2 的 D2（摘流 / 恢复场景）仍未验收。
现在有稳定的 1P1D，是做它的时机。
