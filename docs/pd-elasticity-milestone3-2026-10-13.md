# P/D 自适应与容量动词（milestone 3）— 2026-10-13

> 上一轮（九轮）的结论是「按线上 PD 弹性扩容」**三层都不成立**：负载不是决策输入（而且采不全）、
> planner 没有要容量的动词、实测还会把 1-of-2 放大成全停。本文记录把这三层逐条补齐的实现。
>
> 全部改动只动控制面代码与配置模板，**没有部署**：nightshift 上跑的仍是 `v0.3`、`mode: fixed`。

## 1. #77 先修观测：prefill 的指标名

`collector.go` 的可用性门是 **五个 gauge 全要非 nil**（`Running`/`Queue`/`MaxRunning`/`Prealloc`/`Transfer`），
而 prefill 的 `Prealloc` 取 `num_prefill_prealloc_queue_reqs`——**SGLang 0.5.20 已不再导出这个名字**
（两侧各导出 22 个 `sglang:*req*` 指标，prefill 侧有 `num_prefill_bootstrap_queue_reqs` 而没有 `prealloc`）。
一个改名的 counter 因此让整个 P/D 样本 `Complete=false`，把「所有容量决策的输入」静默关掉。

- `metrics.go` 新增 `valueAny(m, keys...)`：按顺序返回第一个存在的键。
- `collector.go` 按角色构造候选键：prefill 依次试 `num_prefill_prealloc_queue_reqs`、
  `num_prefill_bootstrap_queue_reqs`（**旧名在前，回滚仍可用**）。
- 可用性失败时把**缺失的指标名**写进 `Node.Error`：`缺少调度指标或有效并发上限（缺 …）`。
  原文案「缺少调度指标」曾让一次排查走了三个错误假设。

单测：`TestPrefillGaugeRenameIsAccepted`（两种命名各一次）、`TestMissingGaugeIsNamedInTheError`（点名缺失、不点名存在的）。

## 2. #78 下限保护：不拿走某个角色的最后一个 worker

### 实测事故（10-12，D2 摘流验收）
池内 1P1D 时摘掉 decode ⇒ `Rebalance()` 把**正在 drain 的 decode 排除**在 `input.Serving` 之外 ⇒
`total=1` ⇒ `TargetDecode=1`、`decodePlanned=0` ⇒ Phase 2 把**唯一的 prefill** 改造成 decode ⇒
`0P0D`，**全停 4m14s**（17:14:14→17:18:28），之后自愈，净收益为零。

### 修法
Phase 2 的两个转换分支各加一条：**要转换的 worker 是该角色最后一个时不转换**，改为写 `Notes`。
只有当「本来就会发生一次转换」时才检查，所以 note 永远指向本轮真正的阻塞原因：

- 有可移动候选但它是最后一个 → `refusing to reassign the last prefill worker <id>: a disaggregated pool must keep both roles`
- 候选被 `min_hold` 挡住 → `no movable prefill worker is available for the decode deficit`（原文案）

顺带把两处重复的候选循环收敛成 `nextConversion(...)`。

单测：`TestRefusesToReassignTheLastPrefill`（事故原形：1P、`TargetDecode=1` ⇒ **零动作**）、
`TestFreshLastPrefillIsAlsoRefused`（`min_hold` 路径）、`TestReassignStillWorksWhenOneSpareWorkerRemains`（2P→1P1D 仍要转换，
证明 guard 没有把正常收敛一起挡掉）、`TestPlanNeverEmptiesARole`（12 种池型 × 3 档齿轮的不变式扫描：
**任何角色从 ≥1 变成 0 都算失败**）。

既有 `TestPlannerReassignmentHonorsDrainGrace` 的池子是「单 P 转 D」，正是被禁掉的那种；已改成
**2P 转 1P1D**（最小的合法转换），它要验证的 drain grace 语义不变。

## 3. #79 planner 真正消费 LoadSnapshot

新增档位 `mode: fixed|auto`（**默认 `fixed`，行为与里程碑 1 完全一致**）。auto 档：
`Plan()` 在算目标之前先按测得的压力差走**一级**齿轮阶梯（`1:2 → 1:1 → 2:1` 按 prefill 占比升序）。

| 条件 | 结果 |
| --- | --- |
| 样本不完整（`Valid=false` 或任一压力为 nil） | 保持，note 引用采样器自己的 `advice` |
| 没有任何排队（`QueueLength == 0`） | 保持——**压力差只代表并发占用，不是容量短缺**（采样器的 advice 也是这么说的） |
| `abs(P-D) <= auto_dead_band`（默认 0.2，与采样器建议加容量的阈值同值） | 保持 |
| 已在一端且压力仍高 | 保持 + `already the … end of the gear ladder` |
| 否则 | **移动一级**，并把这一次移动**记为一次变更**（受 `cooldown` 限速） |

三个刻意的保守点：

1. **一次只走一级，且被 `cooldown` 限速**。角色变更要先 drain 再重启，压力要在这之后才会反映出来；
   追着每个样本走等于拿自己的回声当输入。
2. **只有「有请求在排队」才算证据**。只有压力差、没有排队 ⇒ 不动（`TestAutoModeIgnoresAPressureGapWithoutQueueing`）。
3. **档位移动算一次变更**，即使本轮 pool 已经正好是那个形状、没有任何 action。所以
   `LastChangeAt`/冷却/审计都能看到它，不会出现「没人知道是谁改的目标配比」。

可观测：`Decision` 新增 `mode` 与 `effective_gear`（`gear` 仍是人工基准档）；
`GET /v1/capacity/planner`、`console/telemetry` 都回真实 mode + effective gear；控制台徽标由
「固定配比 1:1」变为「自动配比 2:1（基准 1:1）」；档位变化写审计 `planner_gear_change`
（含 from/to/基准档/触发负载）。`PUT /v1/capacity/planner` 可只带 `{"mode":"auto"}` 切档，
带 `gear` 时先设档再切模式（auto 从新基准重新开始走阶梯）。

单测：一次一级（含 `2:1 → 1:1` 而不是直接到 `1:2`）、阶梯走到底、死区边界（gap 正好 0.2 保持、0.21 走）、
无完整样本保持、冷却限速、**负载直接改变分配结果**（4 个 idle：无样本 2/2，decode 重压 1/3）、
`SetGear`/`SetMode` 重启阶梯、以及 fixed 档对极端不均衡样本**零反应**。

## 4. #80 PROVISION / RELEASE：要容量 / 还容量

### 架构约束（决定了形态）
- `planner` 在 `internal/` 内，**不能 import `internal/controller`**（循环）；
- `main.go` 里 `planner.New` 早于 `controller.New`；
- 真正能创建容器的东西（SSH 铺装管道 + 凭据）**在 `api.Server` 上**。

⇒ **planner 只出决定，执行由上层注入**（与既有 `ActionKind` 先例一致）：
`ActionProvision` / `ActionRelease` 进入 `Decision.Actions`，`Controller` 增加可选端口
`CapacityRequester`（`RequestCapacity(ctx, partnerID) (id, error)` + `OutstandingCapacity() int`），
`main.go` 用 `capacityBridge` 把 `api.Server`（`RequestCapacity` / `OutstandingProvisions`）绑上去。
API 侧的创建入口从 HTTP handler 里抽成 `RequestCapacity`，**控制台与自动循环共用同一条创建路径**，
HTTP 状态码经 `provisionErrorStatus` 从三个哨兵错误映射（400/503/409）。

### 触发条件

| 动词 | 条件 |
| --- | --- |
| `PROVISION` | auto 档 + `capacity_actions: true` + 样本完整 + **有排队** + `abs(gap) > dead_band` + **阶梯在该方向已到头** + 本轮**没有移动档位** + **没有未完成的 provision** + 池内至少一个 serving worker |
| `RELEASE` | auto 档 + `capacity_actions: true` + 样本完整 + **无排队** + 两侧压力**都低于 `auto_release_below`（默认 0.1）** + 优先交还**未分配角色**的 idle 实例；无 idle 时交还**压力较低**的角色，且该角色**至少还有 2 个**（永不交还角色最后一个） |

两个 cap 与顺序：
- **比例优先**：本轮移动过档位就不 provision（`TestProvisionWaitsForTheLadderToSettle`）。
  便宜的答案先用，拿「可能已被重排解决」的证据去买容器是不对的。
- **容量动词排在角色阶段之前**：`RELEASE` 选中的实例被登记进 `actions`，后面所有阶段
  （drain/reassign/assign）都会跳过它，同一轮不会既释放又改角色。
- 两者都受 `max_change_per_round` 与 `cooldown` 约束。

### 与 6h `provision_timeout`、排队现实对接
- 决策只发一次：`Input.Capacity.Provisioning` 由执行器（API 的 job tracker）给出，
  **未完成就`capacity held: N provision request(s) are still open`**，不再重发。
  这是「排队中的容器对控制面不可见」唯一可行的处理方式：只有执行器知道自己要过什么。
- 请求是 fire-and-forget：`RequestCapacity` 立刻返回，作业由既有 `runProvision`/`runSelfProvision`
  在 `provision_timeout`（当前部署 6h）窗口内推进；容器真正出现后由 pull/纳管流程接手，
  再被 planner 分配角色。失败（含 `abandon()` 释放）后作业转 `FAILED` ⇒ `Outstanding` 归零 ⇒
  下一个 cooldown 才可能再要一次。
- **默认关闭**（`capacity_actions: false`），这不是偷懒而是平台事实：
  AlayaNeW 上**释放后同名不可重建**（status 212035）、8 卡 EXCLUSIVE 池**可无限期 QUEUED**，
  所以「还容量」是不可逆的运营决定，必须人工开启；开启后 `RELEASE` 也只会降到 1P1D 为止。

单测：默认关闭时零容量动作、阶梯到头才 provision（两侧各一例）、比例优先、有未完成请求就 hold、
优先交还 idle、交还压力较低且有富余的角色、**永不交还角色最后一个**、只要还有排队就不交还、
`capacity_actions` 必须与 auto 档同用（配置校验）、controller 侧注入的 requester 收到一次以
`partner-a` 为名的调用、未接执行器时明确报「无法执行」。

## 5. 测试与配置

`go build ./... && go vet ./... && go test ./...` 全绿。planner 包新增 22 个用例（含扫描式不变式），
controller 新增 3 个，telemetry 新增 2 个。配置模板三份（`deploy/kubernetes/tai-talea.yaml`、
`deploy/*.example.yaml`）补齐 `mode` / `auto_dead_band` / `capacity_actions` / `auto_release_below`，
默认值全部是「与今天部署等价」的那一档。

## 6. 还没做的 / 明确的边界

1. **没有上真机验证**：nightshift 仍是 `v0.3` + `mode: fixed`。要拿到「按线上 PD 弹性扩容」的
   真结论必须部署 v0.4 镜像、把 `mode` 改成 `auto`，然后观察第一次真实档位移动
   （以及 prefill 侧 `available` 是否因 #77 变成 `true`）。
2. **只用压力与排队，没用延迟**。`LoadSnapshot` 已经带 `PrefillLatencyMS`/`DecodeLatencyMS`/
   token rate，但自动决策目前不读：TTFT/TPOT 需要请求侧统计，现在没有可信来源。
   后果是「还没有排队的过载」（例如 decode 并发打满但队列为 0）不会被自动扩容发现。
3. **PROVISION 只能长「已经在跑的池子」**：目标 partner 取自已持有角色的实例；从 0 到 1 台
   仍必须走控制台的创建入口（要凭据）。同一 partner 内多池不区分。
4. **RELEASE 在 AlayaNeW 上不可逆**（名字烧掉 + 可能排队数小时才换回来），因此默认关闭，
   且只降到 1P1D；`MinServing` 之类「最小规模下限」没有做，需要时再加。
5. **拖 drain 路径的同类洞**：`max_serving > 0` 且 `max_serving == 1` 时，`DRAIN` 分支仍可能把某个角色清空
   （`TargetPrefill=0` ⇒ 「超出目标」⇒ 挑中唯一的 prefill）。当前部署 `max_serving: 0`，该路径不可达，
   所以本轮只做 #78 要求的 rebalance 侧；真要开 `max_serving` 时应先拒绝 `max_serving < 2` 的配置。
