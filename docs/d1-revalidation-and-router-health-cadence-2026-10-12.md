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

> **2026-10-12 晚更新：已修。** 编译验证的路走通了（jump host n1 是唯一同时有
> Linux 用户态和外网的机器），改动与验证见 §8。根因比这一条写的深一层：
> `has_uncertain_prefill_transfer()` 的闩锁**全仓没有清除路径**，所以它不只是
> "等得久"，而是"永远等不到"。

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

> **2026-10-12 晚更新：摘流 + 恢复一半已做，PASS，见 §7。**
> 剩余的一半（归还 / release）与在途流量下的摘流还没做，§7.5 写了缺口在哪。

### 5.5 每轮 poll 都在重写实例行（设计如此，但标签曾经说谎）

`pullAuthoritative`（`internal/controller/pull_reconcile.go`）把快照与**当前意图**对齐，
不是只与上一份快照对齐——这正是它"能修一次漏掉的 Push、一份被回退的快照、
以及本地漂移之后没变过的快照"的原因。所以**每一轮它都会为每个实例重发一次
`CAPACITY_UPDATED`**，"什么都没变"才是常规情况。

代价已经量过：nightshift 上每 30s 写 2 行实例 + 2 行审计，`updated_at` 一直在前进
（`LEASE_UPDATED_AT`/`LastSeenAt` 每轮刷新是**有意的**：合作方说"这个容器还活着"
本身就是值得落盘的事实）。

但 `handleCapacityUpdated` 里 `changed` 曾经**无条件**追加 `"spec"`
（只要事件带 spec——而 poll 每轮都带），于是每轮都写一条"spec 变了"的审计。
**已修**（`d366f57`）：改成 `reflect.DeepEqual` 与已存值比较，并在无变化时把 reason 改成
`instance metadata re-confirmed, nothing changed`。没有任何决策读 `"spec"`
（摘流/重注册只看 endpoint/service_endpoint/lease_id），所以行为不变，只是审计不再说谎。

**这个修复尚未进镜像**：新镜像（v0.6）才包含它。

> **修复前的代价已经量到了**（v0.5 在跑，也就是这一版）：在 nightshift 上拉
> `GET /v1/capacity/instances/{id}/audit`，返回的 100 条**全是**
> `capacity_updated {"changed": "spec"}`，最新一条与最旧一条相隔
> `15:37:30 → 16:27:00` = **49.5 分钟**，即 ~29.7s 一条，正好等于 poll 周期。
> 而 `ListAudit` 是 `ORDER BY audit_id DESC LIMIT 100`（`sqlite.go:918`，上限写死 100），
> 所以**一条实例的审计历史实际只有约 50 分钟**：任何早于 50 分钟的运维动作——
> 包括§7 里那次 drain——都会被同一份日志挤出去，`who drained this and when` 从此答不出来。
> v0.6 上线后这个窗口应由"每轮一条"变成"真有变化才一条"。

## 6. 控制面 v0.5 的上线与「重启接管」验证

`d1` 原始记录（§4）用的是 v0.4，且那次是**重启 Router**（成员表清空）才恢复的。
本轮把 §3 的代码修复打成 v0.5 并上线，顺便补上了此前**从未验证过**的一条路径。

### 6.1 部署三步曲

| 步 | 做法 | 为什么必须这样 |
| --- | --- | --- |
| 预检 | 用一个 Pod 跑**新镜像**自己的 `--check-config`，挂宿主真实配置（只读、**不挂 state 卷**） | 二进制没有 shell，`kubectl exec` 进不去也跑不了命令；唯一能执行它的办法是让 Pod 的 command 就是它本身。输出：`configuration /etc/tai-talea/tai-talea.yaml is valid (1 partners, router http://10.214.81.70:8000)` |
| 滚动 | `ns-deploy-cp.py <manifest> --digest sha256:<推送的 manifest>`：**先删旧 Pod 并等它真的消失**，再 apply，再等 Ready | 控制面是 Pod 不是 Deployment。`--wait=false` 紧接着 apply 会和 terminating 对象打架；而一个容器在跑并不说明它跑的是新构建——`imagePullPolicy: Always` 只保证拿到 tag 背后最新的 digest，所以必须把 `imageID` 与推送的 manifest digest **逐字比对** |
| 验收 | `ns-accept-d1.py` | 同 §4 |

本次 `imageID` = `sha256:d49f06920660e0e0d443b76617128cd1b30e9810b8f6299dedebadbffebb3b12`，
与推送的 manifest digest 一致。

### 6.2 重启接管：干净

```
control plane recovery finished  checked=10 lost=0 recovered=0
                                 reregistered=0 resumed_operations=0 resumed_events=0
```

两个实例都保持 `IDLE/SERVING`，角色未动（`172229`=prefill、`172229-1`=decode），
**`start_attempts` 仍是 0**，而且 **`router_worker_id` 与重启前逐字相同**
（`0f953c25` / `038e3175`）——没有重注册、没有 drain、没有换角色。
Router 侧 `prefill_count:1 decode_count:1`、两条 `is_healthy:true`、`/readiness` 200，
经 Router 的推理 3/3 返回带 `usage` 的真实输出。

**但 §5.2 那条路径这次仍然没撞上**：Router 没有重启，成员表与数据库一致，
`settleService` 在第一个分支就返回 `ok`。代码侧已确认顺序是
`FindWorkerByURL` → 角色不符则 `SetReadiness(false)` → `RemoveStaleMembership` → 重注册，
v0.5 的退避与错误信息覆盖的正是这一段——也就是说，**修复在那条路上是"准备好了"，
但还没有被真实触发过**。

### 6.3 一个操作上的坑：控制面地址每滚动一次就变

控制面 Pod **没有 hostNetwork**（Router 有），所以它的地址是集群 IP，
**每次重启都换**：`10.222.49.89` → `10.222.49.107`。
把 IP 写进探针脚本，就会在滚动之后**静默地探测一个已经不存在的地址**
（上一轮留下的常驻观察者就是这样开始打印 `(none)`，而连接超时还把它从 30s 拖到 50s）。

两条纪律：

- 探针一律用 `nsrun.py <driver> --ctrl-pod tai-talea`，运行前用 kubectl 解析 Pod IP
  并注入 `CTRL`；driver 里读 `os.environ["CTRL"]`，**缺失即报错**，不留一个"看起来能用"的旧默认值。
- 长驻观察者**跨不过滚动**（地址在启动时解析一次）。滚动后重启它。
  `ns-watch.py` 现在会在控制面不可达时明确打印 `CONTROL PLANE UNREACHABLE code=…`——
  "问不到"和"没有实例"是两件事，不该都显示成 `(none)`。

若要让地址稳定：h100-04 的 8787 实测空闲（读宿主 `/proc/net/tcp`），
控制面改用 hostNetwork 即可（与 Router 同理）。**本次没改拓扑**，
因为那个地址只被我们自己的探针使用——nightshift 走 pull 模式，
**不存在 worker 回连控制面的路径**。

## 7. D2：摘流 / 恢复验收（PASS）

驱动器 `.workbuddy/tmp/alayanew/ns-d2-drain.py`，跑法
`nsrun.py ns-d2-drain.py --name ns-d2 --ctrl-pod tai-talea --env BUDGET=900`。

**planner 故意留在 1:1**：这样同一个 run 同时覆盖两件事——worker 离池（摘流），
和 planner 发现少一个角色又把它拉回来（恢复）。若去改 gear 让实例"自然 idle"，
就是在摘流的同时动了 planner，观测到的状态变化便不再能单纯归因于 drain。

每个采样点同时读三处：控制面（`instance_state` / `service_state` / `role` /
`router_worker_id` / `drain_deadline_at`）、Router 的 `/workers` 与 `/state/workers`、
以及 `/readiness`；另外每 5 秒发一次**真实推理**。最后一项是必须的：
「关了 readiness 但 Router 还在往里路由」在数据库里看起来和成功一模一样。

### 7.1 外部可观测的时间线（只列变化）

```
t+  0.0s baseline: 172229=IDLE/SERVING/prefill | 172229-1=IDLE/SERVING/decode
                  router: .70:9002 prefill healthy | .67:9002 decode healthy
                  /state/workers 两条都 state=ready | readiness=ready healthy=2
t+  0.0s baseline inference -> 200 tokens=17/12

t+  0.5s target: …-172229-1 role=decode worker=038e3175-118a-489f-b4d7-8de47cb95304
t+  0.5s POST /drain -> 202 {"result":"readiness closed; instance is draining"}
                  …-1: IDLE/DRAINING/decode drain_deadline=2026-10-12T16:18:39
                  .67:9002 state=unavailable reason=manual     ← Router 同一秒就摘了
                  readiness=not ready
t+  0.9s inference -> 503 server_selection_failed
                  "No available decode workers (all circuits open or unhealthy)"

t+  9.4s …-1: DRAINING -> STARTING   （planner 已经把角色补回来了）
t+ 15.8s .67:9002 decode healthy=False
t+196.3s …-1: STARTING -> REGISTERING
t+200.7s …-1: IDLE/SERVING/decode   .67:9002 state=ready reason=-   readiness=ready
t+200.7s final inference -> 200 tokens=17/12
t+203.5s 三连推理 -> 200 tokens=17/12（三次）
```

### 7.2 控制面自己的账（审计，带时间戳）

同一次 run 的审计（`GET …/instances/…-172229-1/audit`），去掉 poll 噪音后是完整的一条链：

```
16:13:39.971 service_draining    {grace_seconds: 300, reason: "operator requested drain",
                                  drain_deadline_at: "2026-10-12T16:18:39Z", pending_release: "false"}
16:13:48.154 service_drained     {exit_code: 0, forced: "false",
                                  router_worker_id: "038e3175-118a-489f-b4d7-8de47cb95304"}
16:13:48.156 service_starting    {attempt: "1", role: "decode"}
16:16:54.853 service_healthy     {role: "decode"}
16:16:54.854 service_registering {role: "decode", router: "primary"}
16:16:59.859 service_serving     {role: "decode", readiness_generation: "13",
                                  router_worker_id: "038e3175-118a-489f-b4d7-8de47cb95304"}
16:16:59.859 planner_ASSIGN      {from_role: "", role: "decode",
                                  reason: "idle instance preferred for the decode deficit"}
```

三点值得指出：

1. **`forced: "false"` 是这一节最强的一条证据。** grace 给到 300s（截止 16:18:39），
   而停服发生在 16:13:48 —— **发起后 9 秒**。所以它不是被 grace 超时强行掐断的，
   是 worker 自己 idle 之后正常停的。`exit_code: 0` 同时说明 SGLang 是正常退出。
2. **`router_worker_id` 在 drain 前后逐字相同**（`038e3175-…`），说明恢复走的是
   **重新注册**而不是换一个身份。
   > **但理由不是当初写的那个。** 本节初稿把这条归因于「worker_id 是 URL 哈希、跨
   > Router 重启不变」，**那是错的**——id 是随机 UUID，Router 一重启全换（§8.5 有
   > 代码与实测两边证据）。这里 id 不变，只证明**同一个 Router 进程**在服务这一对
   > worker，与哈希无关。
3. 审计本身是可用的——**但读的时候要注意顺序**：`ListAudit` 是
   `ORDER BY audit_id DESC`，取 `items[-14:]` 会拿到这一页里**最旧**的 14 条
   （本轮第一次读就是这么被误导的，看起来像"drain 没写审计"）。要看最近动作必须取**头部**。

### 7.3 T4 的三个主张，逐条对照

| T4 主张 | 结论 | 依据 |
| --- | --- | --- |
| readiness 先关闭 / 不再有**新**请求 | **满足** | `POST /drain` 返回 202 的同一秒，Router `/state/workers` 已是 `unavailable / reason=manual`，`/readiness` 变 `not ready`；0.9s 起的新请求全部 503 `No available decode workers` |
| 在途请求结束后才 Stop | **未压到**（见 §7.5） | 发起 drain 时 `load=0`，`finishDrain` 第一轮就推进到停服。`forced:false` 只证明它没被 grace 打断，不证明它等过谁 |
| 停了以后不再有新请求**到达 worker** | **满足** | 从 t+0.9s 到 t+196s（重启）之间每一次探测都是 503"没有可用的 decode worker"，**不是**"连上了但报错"——说明请求根本没到 worker |

### 7.4 恢复，以及 D1/D2 时间差该怎么读

9.4 秒后 planner 就把它重新排成 decode（`planner_ASSIGN … decode deficit`），
200.7 秒后回到 `SERVING` + readiness ready + `prefill_count:1 / decode_count:1`，
三连推理 200 且 `tokens=17/12` 与 baseline **完全一致**。

D1 那次从发起到能推理是 **14m20s**，这次只有 **200s**，差在**恢复的是什么**：
这次容器没被换掉，只是重启了容器内的 SGLang 服务，权重还在 page cache 里；
D1 是新投一个容器（含镜像与权重加载）。**比较恢复耗时前先确认这一点**，
否则会把 7 倍的差距误读成性能回归。

### 7.5 缺口（未做，不要在别处当成已验）

1. ~~**有在途流量时的摘流没做。**~~ **已补，见 §9。** 本节发布时 drain 恰好撞上 idle，
   所以 T4 的中间那条分支（"等请求跑完再停"）没被压到。§9 用 4 个 2048-token 长请求
   把 decode worker 打满后再摘，4/4 完整返回、等满 25.8 秒才移除。
   （`forced=true` 强制停那条分支仍未触发——要触发它得让请求跑过 300s grace。）
2. **归还（release / reclaim）那一半没做。** D2 的验收口径是「摘流 / **归还** /
   控制面重启恢复各一遍」，其中控制面重启恢复已在 §6.2 验过，摘流在本节，
   归还仍缺。它是三者中唯一**破坏性**的（实例真的没了，要靠 planner 重新投一个）。
3. **§5.2 的 stale-role 路径依旧没被真实触发**（本轮没有角色变更）。

## 8. Router fork 修复 + v0.3 上线（2026-10-12 夜）

§5.1 那条"应该修"在这一节兑现，并且带回来一条**与 §7.2 相冲突的旧结论的纠正**（§8.5）。

### 8.1 根因比 §5.1 写的深一层：不是"等得久"，是"永远等不到"

`Duration::MAX` 只是放大了下面这件事，它本身不是根因。

`Worker::mark_prefill_transfer_uncertain()`（`worker.rs:915`，实现委托给 base worker
`worker.rs:1227`）在 `src/` 下**只有一个调用点**：`routers/http/pd_router.rs:192` 的
`PrefillPhaseGuard::drop()`，且只在 `phase == Dispatched` 时触发——也就是**一个 prefill
请求在 KV 传输途中被取消/断开**的时候。

而 `worker.rs:192-195` 把它的语义写得很明确：

> This is a **sticky safety latch**: draining must not remove the worker until an
> **operator resolves** the ambiguous transfer.

问题是：`grep -rn "prefill_transfer_uncertain" src/` 的全量输出里**没有任何清除/复位路径**。
那个 `until an operator resolves it` 没有任何可执行的兑现手段——闩锁一旦置位，只有
两个东西能把它抹掉：**重启 worker 进程**，或**重建 worker 对象**（`worker_builder.rs:214`
会把它初始化成 false）。两者都不是"operator resolves the transfer"。

所以链路是：一个被取消的 prefill 请求 ⇒ 闩锁永久置位 ⇒ `wait_until_idle` 永久 false ⇒
`drain_pd_workers` 在 `Duration::MAX` 上永等 ⇒ 所有 `depends_on` 它的删除步骤永远不跑。

**结论：不改闩锁语义**（它是刻意的 safety latch，放松它会让"KV 可能没传完"被静默掩盖），
**而是让失败变得有界、有声、可诊断**。

### 8.2 改了什么（5 个文件）

`src/core/steps/worker/local/drain_pd_workers.rs`（整文件重写）

* **两套独立预算**，这是整次改动里唯一容易写错的地方：

  | 常量 | 值 | 计什么 |
  | --- | --- | --- |
  | `PD_DRAIN_LOAD_BUDGET` | 300s | **在途请求**（`load != 0`），从 step 开始计时 |
  | `PD_DRAIN_UNCERTAIN_BUDGET` | 15s | **含糊 transfer**，从 worker **变 idle 那一刻**开始计时 |
  | `PD_DRAIN_STEP_TIMEOUT` | 390s | step 自身超时（> 两套预算之和，留出收敛余量） |
  | `PD_DRAIN_POLL_INTERVAL` / `PD_DRAIN_LOG_INTERVAL` | 50ms / 10s | 轮询 / 进度日志 |

  含糊预算**必须在 worker 变 idle 时才开始**（`ambiguous_since: Option<Instant>`，
  与 `started` 分开）。若与 `started` 共用一个时钟，一个已经忙了 20 秒钟的 worker
  会**在它刚 idle 的那一瞬**立刻超时——测试
  `ambiguity_grace_starts_when_the_worker_goes_idle_not_at_step_start` 就是钉这个时序陷阱的。

* **阻塞原因是稳定标签**，不是自由文本：`enum Blocked { InFlight{url,load},
  AmbiguousPrefillTransfer{url} }`，`reason()` 返回 `"in_flight"` /
  `"ambiguous_prefill_transfer"`（给指标用），`describe(waited)` 返回人话（给人看）。
  每 worker 每 10s 一条日志；含糊那条额外**只记一次**
  `Metrics::record_pd_drain_blocked_uncertain`，避免刷爆。

* **等不下去时返回失败，worker 保持注册**：`Err(WorkflowError::StepFailed{step_id:
  "drain_pd_workers", ...})`，并逐个 `record_pd_drain_incomplete(url, reason)`。

`src/core/steps/worker/local/mod.rs`

```rust
.with_timeout(PD_DRAIN_STEP_TIMEOUT)                     // 原为 Duration::MAX
.with_retry(RetryPolicy { max_attempts: 1,
                          backoff: BackoffStrategy::Fixed(Duration::from_secs(0)) })
.with_failure_action(FailureAction::FailWorkflow)        // 显式钉住，不靠默认
```

> `PD_DRAIN_STEP_TIMEOUT` 是 `pub(crate)`，**不能 `pub use` 重导出**（编译失败过一次），
> 只能 `use`。原有测试 `worker_removal_drain_step_has_no_timeout` 相应改名为
> `..._has_a_finite_timeout`，断言从"等于 `Duration::MAX`"反转为"等于 `PD_DRAIN_STEP_TIMEOUT`"。

`src/core/job_queue.rs` — 新增 `publish_job_detail(worker_url, status, message)`

`status_map` 是 `GET /workers/{id}` 渲染 `job_status` 的唯一来源（`worker_service.rs:355`）。
这就是 §5.1 里要求的"把还在等这件事暴露出去"。实现故意**走 crate 自带的构造器再改字段**
（`JobStatus::processing(...)` 然后覆写 `status`/`message`），而不是直接字面量构造——因为
`JobStatus` 来自外部 crate `openai-protocol`（字段全 `pub`、构造函数只有
`pending/processing/failed`），这样外部字段一变动，我们受影响的面最小。

另外 `publish_job_detail` 每次都刷新 `timestamp`，顺带**压住** `cleanup_old_statuses`
的 5 分钟 TTL（每 60s 跑一次，`retain(|_, v| now - v.timestamp < STATUS_TTL)`）——
否则一条长时间的 drain 会在第 5 分钟从 `job_status` 里消失。

`src/observability/metrics.rs` — 新增 `smg_pd_drain_incomplete_total{worker, reason}`

与既有的 `smg_pd_drain_blocked_uncertain` **刻意分开**：被阻塞是**状态**（还在发生），
排不干净是**结果**（已经失败）。混在一个计数器里，就再也答不出"这种阻塞最后到底有没有
收敛"。

`src/core/worker.rs` — 只加文档：在 `mark_prefill_transfer_uncertain` 上写明这个闩锁
目前是**单向**的、crate 内无清除路径，所以 drain 选择"失败"而不是等一个不会到来的状态。

### 8.3 编译验证：把 n1 当构建机

本机（Windows）没有 Rust，nightshift 的节点是 air-gap ⇒ 能同时满足
"Linux 用户态 + 外网"的只有跳板机 **n1**。这条路与"在集群内打镜像"同构：在 n1 上编出
二进制，再送进集群。实测 n1 具备：Ubuntu 26.04 / kernel 7.0.0-29 / glibc 2.43 /
4 vCPU / 7GB / 82G 空闲；rustup stable **`rustc 1.99.0 (b940084d7 2026-09-28)`**；
crates.io 与 github.com 均通。

工具链脚本在 `.workbuddy/tmp/alayanew/`：`n1-rust-env.sh`（依赖）、`n1-build.sh`（编译）、
`n1-test.sh`（测试）、`n1-rootfs.sh`（rootfs）、`smg-router-image.py`（组镜像 + 推送）、
`n1-prepare-rust.py`（上传/启动/看日志的总控）。

三个踩过的坑，都写进了脚本：

1. **第一次构建失败是漏了 `libprotobuf-dev`**。`smg-grpc-client` 的 build script 要
   `google/protobuf/timestamp.proto`，well-known types 由它提供。现在脚本里加了断言
   （缺 `timestamp.proto` / `struct.proto` 就直接报错退出），不再等 cargo 报。
2. **`apt-get update` 在这台机器上非 0**：内网源 `mirrors-internal.cmecloud.cn` 的
   `Components-amd64.yml.xz` **Hash Sum mismatch**。所以脚本**绝不能 `set -e`**，
   否则会在装任何东西之前就中断。（同上，n1 上真正可用的是直连的 `archive.ubuntu.com`。）
3. **`setsid nohup ... &` 不能配 `recv_exit_status()`**：sshd 会因为后代仍持有 channel
   而不返回，把整个工具拖到超时被 SIGTERM（连续两次、零输出）。改成
   `open_session()` + `chan.exec_command` + `recv(4096)`，**读一行就 close**。
   顺带：日志必须分成 `rust-build.log` / `rust-test.log` / `router-rootfs.log` 三份——
   启动动作会先 truncate，共用一份会互相清空（整包测试日志就是这么丢的）。

构建结果（`cargo build --profile ci --features vendored-openssl --bin sgl-model-gateway`）：

* 产物 `target/ci/sgl-model-gateway`，**74,371,784 B**；首次 **16m44s**，增量 **4m12s**，rc=0。
* 用 `--profile ci` 而不是 `release`：**部署中的 v0.2 就是 ci profile 编的**，同 profile 才可比。
* `objdump -T | grep -o 'GLIBC_[0-9.]*' | sort -Vu | tail` → 最高 **`GLIBC_2.39`**
  ⇒ **排除 `bookworm`(2.36)**，选 `noble`(2.39)，与 v0.2 同底座。

**同一份源码、同一个 profile，两次构建 sha256 不同**（`083c0bb9…` → `ed272946…`，
BuildID 也从 `1220d378` 变 `6a9c115f`）。这是"**同一 tag 不代表同一份字节**"纪律的直接证据，
也是 §8.4 里必须逐字比对 imageID 的原因。

### 8.4 镜像与上线

rootfs：`debootstrap --variant=minbase --include=ca-certificates,curl,iproute2 noble`
→ **151M**，内部 `(Ubuntu GLIBC 2.39-0ubuntu8) 2.39`（正是 §8.3 判据要求的），chroot 冒烟通过。

* `assemble`：二进制落 `/usr/local/bin/sglang-router`（70.9 MiB）+ 建 `sgl-model-gateway`
  symlink + `/tmp`(1777) + `/log`。
* `verify`：在 rootfs 内 `chroot /usr/local/bin/sglang-router --version` → rc=0，
  打印 **`sgl-model-gateway 0.3.2`**。
* `build`：raw **170.4 MiB** / gz **66.3 MiB**，`diff_id sha256:77cf73642b94`，
  `layer sha256:f8ddbc0d5179`。
* `push` → **`library/talea-router:v0.3`**，manifest digest
  **`sha256:cc49e5a0920d42eb20edf98ded4faa6f91c9acd4f19582b1bbddb1daa76d7b28`**。

> 凭据（`TALEA_REGISTRY_USER` / `TALEA_REGISTRY_PASSWORD`）只在仓库**外**的
> `.workbuddy/tmp/alayanew/registry-creds.env` 里，**不进命令行**（远程 shell 留 history，
> `ps` 对所有用户可见）。`tai-core/tai-talea` 是公开仓库，**提交前必做凭据扫描**。

滚动：`ns-deploy-router.py deploy/kubernetes/talea-router.yaml --digest sha256:cc49e5a0…`。
Router 是 Pod 不是 Deployment，且 `hostNetwork: true` + `hostPort: 8000`，所以
**必须先删旧 Pod 并等它真的消失**再 apply（否则新 Pod 抢不到端口）。

**落地证据（不靠 tag）**：

```
image     = .../library/talea-router:v0.3
imageID   = .../library/talea-router@sha256:cc49e5a0920d42eb20edf98ded4faa6f91c9acd4f19582b1bbddb1daa76d7b28
phase     = Running ready = true restarts = 0
startedAt = 2026-10-12T17:06:12Z
### MATCH: the running Router is the build that was pushed
```

**滚动后复验（`ns-state.py`，`--ctrl-pod tai-talea`）——与滚动前逐项一致**：

```
控制面   172229=IDLE/SERVING/prefill a=0 | 172229-1=IDLE/SERVING/decode a=0
Router   stats={prefill_count: 1, decode_count: 1, regular_count: 0, pending_count: 0}
         .70:9002 prefill healthy=True load=0 | .67:9002 decode healthy=True load=0
state    /state/workers 两条都 state=ready
ready    /readiness 200 {healthy_workers: 2, available_workers: 2, total_workers: 2}
推理     3/3 -> 200 tokens=17/12
```

单测：整包 **`557 passed; 0 failed; 0 ignored`**（rc=0，19.66s）；过滤跑 drain 相关
**10 passed / 0 failed**，含 5 个 `drain_pd_workers::tests` 与
`worker_removal_drain_step_has_a_finite_timeout`、以及既有的
`test_same_url_register_preserves_draining_state`。

### 8.5 这条被纠正的旧结论：worker id **不是** URL 哈希

本轮重建 Router 是**第一次有机会看到 Router 重启后 id 会怎样**，结果推翻了 §7.2 初稿的归因。

代码（`src/core/worker_registry.rs`）：

```rust
pub struct WorkerId(String);
impl WorkerId {
    pub fn new() -> Self { Self(Uuid::new_v4().to_string()) }   // :212 —— 随机 UUID
}
```

`WorkerRegistry::register()`（`:525-534`）：

```rust
let existing_id = self.find_equivalent_worker_id(worker.url());
let worker_id = if let Some(existing_id) = existing_id { existing_id } else { WorkerId::new() };
```

`find_equivalent_worker_id` 用 `worker_registration_key(url)`（`worker.rs:497`，
就是归一化后的 URL）去查 **`self.url_to_id`** —— **一个进程内的 DashMap**。
注册接口 `POST /workers -> 202 {worker_id, url}`（`internal/routeradapter/client.go:6`）
是**服务端生成 id、控制面存下来**。

⇒ 两个真实推论，方向相反，都重要：

1. **Router 一重启，id 全换**（`url_to_id` 空 ⇒ 每个 worker 都 `WorkerId::new()`）。
   本次实测：`0f953c25→035c6795`、`038e3175→b32f747f`。
   所以 Router 重启会**丢掉** manual drain 状态（新 id、新条目，`state=ready`），
   而不是"粘住"。
2. **同一个 Router 进程内，同 URL 重新注册会复用 id** ⇒ §5.2 说的"manual drain 粘住"
   **是真的**，但机制是**进程内的 URL 键**，与"URL 哈希跨重启不变"无关。
   `test_same_url_register_preserves_draining_state` 就是这个语义的单元测试。

所以正确的表述是：**URL 是"同一个 worker"的判据（进程内），id 是随机且只属于这一次注册**。
两条容易被合并成一条记，而合并之后就会推出"重启后 id 不变"这种错误的预测——
§7.2 初稿正是这么错的。

## 9. 验证 v0.3：把没人跑的那一步跑起来（PASS，代价见 §9.1）

驱动 `.workbuddy/tmp/alayanew/ns-v03-remove.py`，跑法
`nsrun.py ns-v03-remove.py --ctrl-pod tai-talea --timeout 1200`。

### 9.0 为什么必须人为触发

§8 改的是 `drain_pd_workers`，而它是 Router `worker_removal` 工作流的一步
（DAG：`find_workers_to_remove → drain_pd_workers → remove_from_policy_registry →
remove_from_worker_registry → update_remaining_policies`）。这个工作流只由
`DELETE /workers/{id}` / `DELETE /workers/by-url`（`server.rs:884-887`）启动，而
**控制面刻意从不调用它们**：它摘流靠 `PUT /state/workers/{id}`，从不用 DELETE
（`acceptance_test.go` 里有两条断言专门钉这一点："draining must not use DELETE /workers"）。

⇒ **常规运行不会碰到这一步。** 部署一个没人跑的修复等于没验证，所以直接用 Router
自己的删除 API 驱动它。方法：先起 4 个 2048-token 的真实长请求打 decode worker，
等 Router 自己报 `load > 0`，再发 DELETE。

### 9.1 证据一：Router 日志（v0.3 那行确实在执行）

```
17:14:35 WARN smg::core::steps::worker::local::drain_pd_workers:
           src/core/steps/worker/local/drain_pd_workers.rs:254:
           PD drain is waiting for in-flight requests
           worker_url=http://10.214.81.67:9002 load=4 waited_secs=20
17:14:40 ... Step succeeded step_id=drain_pd_workers duration_ms=25787
17:14:40 ... Step started  step_id=remove_from_policy_registry
17:14:40 ... Removed worker http://10.214.81.67:9002
17:14:40 ... Step succeeded step_id=remove_from_worker_registry
```

`load=4` 正是探针并发的那 4 个请求；`duration_ms=25787` 是有界等待真的等满的时长；
`:254` 是新代码里那条 `warn!`。**这行日志在 v0.2 里不存在**（那时是静默死等）。

### 9.2 证据二：`job_status` —— §5.1 要求的新出口

`GET /workers/{id}` 在 drain 期间逐次刷新（每次都刷新 `timestamp`，所以没被 5 分钟
TTL 清掉）：

```
t+ 0.6s {"job_type":"RemoveWorker","status":"draining",
         "message":"still serving 4 in-flight request(s) after 0s; the drain budget is 300s"}
t+10.6s ... after 10s ...
t+20.7s ... after 20s ...
t+26.7s {}          ← 工作流完成，条目被清
```

**这是"把还在等这一状态暴露出去"真正落地的样子**：以前这里在 drain 期间是空的。

### 9.3 证据三：在途请求本身 —— 顺带补上 §7.5 缺口①

```
#0 began=t+0.6s ended=t+26.3s code=200 completion_tokens=2048
#1 began=t+0.6s ended=t+26.4s code=200 completion_tokens=2048
#2 began=t+0.6s ended=t+26.4s code=200 completion_tokens=2048
#3 began=t+0.6s ended=t+26.4s code=200 completion_tokens=2048
removed at t+26.7s
```

4/4 返回 200 且 `completion_tokens=2048`（**全部写满，没被截断**），最后一个在
`t+26.4s` 结束，worker 在 `t+26.7s`（0.3 秒后）才从池里消失。

⇒ **T4 的中间主张「在途请求结束后才 Stop」这次真的被压到了。** §7 那次因为恰好 idle
而没测到，`forced:false` 只能证明没被 grace 掐断；这次是"等满 25.8 秒、4 个请求全部
完整返回、然后才摘"。

（机制上正确：`drain_pd_workers` 先把 worker 移出 PD 策略再等，所以等待期间不会有新
请求进去——它等的是已经发出的工作，不是新流量。）

### 9.4 ⚠️ 这次触发暴露的真问题：planner 会把 1-of-2 的降级放大成全停

**这是本轮唯一一次生产影响，必须写清楚。** 我删掉 decode worker 之后，planner 的反应
不是等，而是**把唯一的 prefill 也改造成 decode**：

```
17:14:24.314 service_draining  {reason: "planner rebalance prefill -> decode", grace_seconds: 300}
17:14:24.315 planner_REASSIGN  {from_role: "prefill", role: "decode",
               reason: "no idle capacity left; rebalance a prefill worker to decode"}
17:14:36.202 service_drained   {exit_code: 0, forced: "false"}
```

池子里没有第三个空闲实例，所以"补 decode 缺口"的唯一手段就是**拆掉 prefill**。完整时间线：

```
17:14:14  我发 DELETE /workers/by-url（decode）        ← 池子变成 1P0D
17:14:14  drain_pd_workers 开始等在途（load=4）
17:14:24  控制面发现 decode worker 不可路由（下一轮 reconcile）
17:14:24  planner：no idle capacity left → rebalance prefill -> decode
17:14:36  prefill 服务停（forced:false、exit 0；它此时已 idle）
17:14:40  4 个请求全部 200 返回；decode worker 从 Router 移除  ← 池子变成 0P0D
17:14:43  decode 重新注册（新 id ae8f3a63）
17:15:32  prefill service_starting（attempt 1，角色又变回 prefill）
17:18:23  prefill healthy → registering
17:18:28  prefill service_serving（gen 13）
17:18:28  planner_ASSIGN {reason: "idle instance preferred for the prefill deficit"}
```

**全停约 4 分 14 秒（17:14:14 → 17:18:28），然后完全自愈，无人干预。**
恢复后两侧 `IDLE/SERVING` a=0、`prefill_count:1 / decode_count:1`、两条 `healthy=True`、
`/state/workers` 两条 `ready`、`/readiness` 200、推理 3/3 `tokens=17/12`。

一来一回的净效果是 **零收益 + 4 分钟停机**：等它把 prefill 停掉再启动时，decode 已经
自己回来了，于是 planner 又把它排回 prefill。这是设计问题不是实现 bug——
`no idle capacity left` 时"rebalance 另一个角色"，在一个没有富余的池子里等价于
"**把当前唯一的另一种能力也拿掉**"。planner 缺的信息是"**这个缺口可能只是瞬时的**"
（我们这次移除是刻意的、短命的动作，Router 成员表正在重建），它把瞬时缺口当成了稳态
缺口。这与 §5.3 是同一类问题：瞬时条件被当成稳态条件处理。

> 低风险的修法方向（未做）：池内只剩一个某角色实例时，禁止把它 rebalance 成另一种角色；
> 或者先等一个 reconcile 周期确认缺口仍在。判据不是"要不要 rebalance"，
> 而是"**rebalance 的代价是不是要拿掉最后一份已有能力**"。

### 9.5 一个次要观察：`pd_state=draining` 会短暂粘到重新注册的 worker 上

decode 实例的审计里有两条：

```
17:14:24.311 service_start_failed {attempt: "0", exhausted: "false",
               error: "router worker is permanently unroutable: pd_state=draining (healthy=true)"}
17:14:38.261 service_start_failed {同上}
17:14:43.991 service_reregistered {readiness_generation: "12", router_worker_id: "ae8f3a63"}
```

worker 被移除后重新注册，`pd_state=draining` 一度还在（正是
`test_same_url_register_preserves_draining_state` 钉住的语义），控制面把它判成
"permanently unroutable"。**两点值得注意**：① 它几十秒后自己恢复了，所以
"permanently" 这个词与事实不符；② `attempt: "0"` + `exhausted: "false"` 说明
**没有烧启动预算**——这点设计是对的，否则一次瞬时的成员表重建就能烧掉 1/3 的预算。

### 9.6 顺带证实 §8.5（又一次）

* decode 重新注册后 id 从 `b32f747f` 变成 `ae8f3a63`（**新随机 UUID**）。
* prefill 的 id `035c6795` **保持不变**——因为 Router 进程没重启、URL 也没变，
  `register()` 只是把它重新排了一下角色。

同一次运行里同时出现"id 变了"和"id 没变"，正好是 §8.5 那两条推论各自的一次实例。

### 9.7 探针自己判 INCOMPLETE，是窗口给短了

`ns-v03-remove.py` 的判据 6（恢复后能推理）报了 `False`：它的恢复窗口是
`BUDGET + RECOVER_BUDGET = 420 + 240 = 660s`，于 `t+208.9s`（约 17:17:42）放弃，
而 prefill 在 **17:18:28** 才回到 SERVING——**早了约 46 秒**。
六条判据里前五条全 True（删除被接受、`job_status` 有界等待、4/4 完整、移除在请求之后、
控制面重新注册）。下一次跑把 `RECOVER_BUDGET` 给到 420s 以上即可。
