# 自愈的第二半：轻探针 + 重试预算（2026-10-15）

> 承接 `automation-status-2026-10-14.md`（四层自动化能力盘点，其中第 ④ 层「自愈」标为 ❌）
> 与 `nightshift-outage-and-host-contention-2026-10-14.md`（worker 冻住那一晚的取证）。
> 那两篇解释**为什么做不到**；本篇交付**代码**，并如实标出**哪些还不能立刻生效、为什么**。
> 时间一律 **UTC**（CST = UTC+8）。

---

## 0. 结论速览

| # | 项 | 结果 |
| --- | --- | --- |
| 1 | 交付 | 两半：**B** 让 k8s 自己重启冻住的容器（bootstrap 轻探针 + `livenessProbe`）；**A** 打开控制面自己关上的三道门（重试预算冷却重臂、停摆实例对 planner 可见、LOST 死角判据） |
| 2 | 代码回归 | `gofmt`/`go vet`/`go build` 全绿；`go test ./... -count=1` **16 个包全绿**（新增 25 条 Go 测试）；`python -m unittest` **77 项通过 / 2 跳过 / 0 traceback**（新增 4 条） |
| 3 | 存储迁移 | schema **v3 → v4**，`capacity_instances` 加两列（`start_rearms`、`start_parked_at`），已有行保持原值（`0` / `NULL`），迁移测试覆盖 v3 库升级 |
| 4 | 默认行为 | **不改变**。B 的探针是 opt-in（镜像没有该端点时会 CrashLoop）；A1 的冷却默认 `0`（= 今天的行为）；A2/A3 只改判据与可见性，不新增破坏性动作 |
| 5 | 立刻可生效 | A2/A3 随控制面镜像上线即可；A1 需改两行配置；**B 需要先重建 worker 镜像**（端点随 bootstrap 进镜像），且**已运行的 Pod 不会被改动** |
| 6 | 刻意不做 | **不**自动释放「还列在伙伴清单里但不应答」的容器；**不**把停摆实例计入 planner 的 serving/total；**不**在冷却外无限重试 |
| 7 | 仍未闭环 | ① 九章/SSH 伙伴那边**没有**容器级重启原语（B 只治 k8s），冻住仍只能靠 SGLang 自带 watchdog 或人工；② B 在 nightshift 的**真机验证**要等镜像重建 |

---

## 1. 为什么是这两半

两类故障形态不同，修复点也不同，所以是两半而不是一件：

| | 故障 | k8s 管不管 | 控制面管不管 | 修哪一边 |
| --- | --- | --- | --- | --- |
| **B** | 容器活着、占着 8 张卡、HTTP 面**不再应答**（NVRM 卡死指纹） | ❌ 进程没退、无 owner、无 livenessProbe ⇒ 永不重启 | ❌ 摘流 + 告警，**刻意**不动手 | k8s manifest：给它一个探针，让 kubelet 动手 |
| **A** | 容器/平台侧的**一次性失败**（卡被别人占、镜像库不可达、权重还在写） | — | ⚠️ 会重试，但预算烧完**永久停摆**，而 planner 又看不见它 | 控制面：冷却重臂 + 可见性 + 判据 |

关键约束（决定了两半的边界）：

- **`markLost` 的注释是设计契约**：*Bootstrap loss does not prove the model is dead.* 网络分区与进程死亡长得一模一样，贸然动作会杀掉只是被隔开的健康模型。所以「容器不应答」这一条信号**不足以**支撑任何破坏性动作（释放、重建）。
- **`/bootstrap/health` 是模型的代理，不是一个「我在活着」的信号**：它持服务锁、再以 2 秒超时问模型的 `/health_generate`。于是「bootstrap 忙」「模型冻住」「bootstrap 死了」从外面看**完全一样**。B 的探针必须换一个端点，不能复用 health。

---

## 2. B1：bootstrap 的轻量存活端点

`bootstrap/tai_talea_bootstrap/server.py`

新增 `GET /bootstrap/liveness` —— **不持任何锁、不碰模型、不碰磁盘**，只回：

```json
{"status": "alive", "version": "tai-talea-bootstrap/1", "uptime_seconds": 42}
```

- 为什么不是 `/bootstrap/health`：health 是**模型**的代理，加载期会失败、`PHASE_FAILED` 时返回 503。拿它当 liveness 会**反复杀死正在正常加载的容器**。
- 为什么安全（不会误杀加载期）：模型是 `subprocess.Popen` 起的**子进程**（`service.py:305`），bootstrap 自己的解释器在加载期间是空闲的；`_start` 只在极短临界区持锁，`prepare()` 与模型加载都在锁外；HTTP 服务器是 `ThreadingHTTPServer`，一个阻塞的 `health()` 不会占用其他请求。所以 liveness 在环境准备期、模型加载期**都秒回 200**，只在「进程不再服务 HTTP」时才沉默。
- 鉴权：`_authorized()` 在所有 GET 之前检查，新端点天然需要 `X-Bootstrap-Token`；测试覆盖了 401 与「版本/uptime 字段」。
- 测试里最强的一条是**双向断言**：后台线程调 `service.health()`（会阻塞在服务锁上），主线程持锁时 liveness 必须 200，且断言 health **不能**在 0.3s 内完成；释放锁后 health 必须完成。这把「liveness 真的不依赖模型」证成了事实而不是注释。

---

## 3. B2：worker Pod 的 livenessProbe

`internal/partners/kubernetes/{manifest.go,client.go,adapter.go}` + `internal/config/config.go`

`buildPod` 在 `ReadinessProbe` 之外加一条 `LivenessProbe`，指向 `/bootstrap/liveness`，带 `X-Bootstrap-Token`。时序是**常量**（不开放配置，理由写在 `manifest.go`）：

| 项 | 值 | 理由 |
| --- | --- | --- |
| `initialDelaySeconds` | 60 | 覆盖 Python + entrypoint 起来；6 倍于实测 bring-up |
| `periodSeconds` × `failureThreshold` | 10 × 6 = **60s** | 一个平时微秒级应答的服务器**整整一分钟不答**已经没有歧义；而触发它的那次冻结持续了 **10 分钟** |
| `timeoutSeconds` | 5 | 与 kubelet 报的 `context deadline exceeded` 同一个量级 |

**为什么是 opt-in（`kubernetes.liveness_probe`，默认 false）**：探针指向的端点必须存在于**worker 镜像**里。旧镜像会回 404，kubelet 读作「探测失败」⇒ 容器**永远重启、永远起不来**。一个冻住的 worker 很糟，一个永远起不来的 worker 更糟。开关与镜像 tag 是同一个镜像契约的两半，必须一起动。

**生效范围**：探针写在 Pod spec 里，**创建时固定**。已经跑着的 worker **不会**被改动 —— 在 nightshift 上启用它需要重建一次 worker Pod（一次 drain + 重载，约 7 分钟）。这一点必须让操作者知情。

**它等价于什么**：探针触发的重启 = 人工 `kill 1`：**同 Pod、同地址、同租约、`restartCount++`**。零权限变更（不需要 exec/portforward）。

---

## 4. A1：烧完的启动预算，冷却后重臂

`internal/{config,domain,store,controller}`

**问题**（`RequeueService` 的注释早就写明了）：预算烧完后 `reconcile.go` 直接 `return nil` 永久放弃；而这一行**既是重试的终点、也是 planner 的盲区** —— 行上还挂着有效角色（不是 idle），FAILED 又不是活服务（不是 serving），状态机两边同时关闭，只剩 drain/release/reclaim 这些**会毁掉容量的**动词。

**新增配置**（默认全 `0` ⇒ 行为不变）：

```yaml
controller:
  start_retry_cooldown: 30m    # 0 = 关闭（默认）
  start_retry_max_rearms: 3    # cooldown > 0 时必填，否则加载失败
```

`start_retry_max_rearms` 是**必填而不是默认值**：一个真的坏掉的启动（模型路径错、bootstrap 拒绝参数）会在每个冷却窗口**重载 8 张卡的权重**，无限重试比它要治的那次故障更糟。烧完 N 次仍从未 SERVING ⇒ 停摆等人，`RequeueService` 是那个动词。

**冷却时钟用的是哪个时间戳**（这一步试错了两次，结论值得记下来）：

| 候选 | 为什么不行 |
| --- | --- |
| `instance.LastSeenAt` | **被伙伴观察刷新**：pull 循环对「没有变化」的容器也会重申一次并写回（`events.go:343`）。冷却永远不会到期 —— 恰恰在最需要它的伙伴上。 |
| `operations.updated_at` | store 用**真实时钟**写这一列，而控制器用注入的 `c.now()` 判定；两者在部署上可以不一致。 |
| `operations.created_at` | **冲突时不更新**（upsert 的 DO UPDATE 里没有它），所以它是最初那次 start 的时间，不是失败时间。 |

⇒ 新列 `start_parked_at`，由 `failStart` 在**烧完预算的那一刻**用控制面自己的时钟写入；重臂时清零，于是「一个冷却窗口最多重臂一次」由构造保证。`start_rearms` 是配套的计数器，服务一旦 SERVING 就清零（否则一个长期存活的实例会攒下一笔债，下次瞬时故障就被永久停摆）。

**为什么不在同一轮里直接重试**：重臂只改行 + 重写 operation（`NextRetryAt` 归零），下一轮通过**和首次启动完全相同的那条路径**发起尝试。这样「启动一个服务」只有一处代码，「attempt 计数」只有一处自增。

---

## 5. A2：停摆实例对 planner 可见

`internal/{planner/planner.go,controller/rebalance.go}`

`planner.Input` 新增两个桶，`Decision` 新增两个计数：

- **`Recovering`** —— 有角色、不在服务，但**不需要人**就会再试：启动预算没花完，或配了冷却且重臂次数还有，或（LOST）准备管线还有预算。
- **`Stranded`** —— 同样形态，但**没有任何东西会再试它**：预算烧完且无重臂；或 LOST 且准备预算也烧完。这是那条升级告警要人来回答的状态。

它们带来的行为变化只有一处，但是关键的一处：**`PROVISION` 在 `Recovering` 非空时被抑制**。队列压力分不清「一个 worker 正在重启」和「一个 worker 从来没被订过」，混淆的代价是：九章上多一个排队数十小时的创建，H100 上**八张卡被占用两次**。抑制窗口由构造有界（`Recovering` 只持续到「还会再试」为止），一旦实例 SERVING 或转为 `Stranded`，闸门就打开 —— 停摆实例是池子上的洞，必须用别的方式补（换一台机器，或人来处理），**不能**靠压制申请把它变成永久故障。

**刻意不做**：

- **不**把它们计入 `input.Serving`（会虚增 `ServingCount`、污染 `CurrentRatio`，并对失败实例下 `ActionReassign`/`ActionDrain`）。
- **不**把它们计入 `total`（差口会被「转换一个健康 worker」去填 —— 这正是 10-13 那次把 1-of-2 的降级放大成全停的机理）。
- **不**把它们放进 `Idle`（`assignable` 的注释仍然成立：失败启动由 reconciler 重试，**不是**让 planner 去抢）。

---

## 6. A3：LOST 记录可清理 —— 结论与一处真修正

原先设想「LOST 超过 grace 后允许重新走准备流程」。读代码后结论是：**自动清理已经存在**，缺的是判据。

- ✅ **伙伴不再列出的容器会自动清理**。pull 路径 `pull_reconcile.go:98-107` 对「快照里没有、且非 pending_release」的活跃实例发 `CAPACITY_REVOKED`；`handleCapacityRevoked` 走 `RequestRelease`；`finishDrain` 对 LOST **不调 `/bootstrap/stop`**（端点不答）直接记 stopped（`lifecycle.go:1012` 有注释），再到 `ReleaseInstance`，而 `ErrInstanceNotFound` 按契约当成功。**这条链路本轮用测试钉住了**（含「不得对哑容器发 stop」与「重复 revoke 幂等」）。
- ✅ **停在那里但不应答的容器，人一个动词就能还**：`RequestReclaim` → LOST 感知的 drain → RELEASED，不会卡在 DRAINING。也已用测试钉住。
- ⚠️ **真修正**：`runPrepare` 在健康探针一直不答时会**把行停在 LOST**、并把 prepare operation 标成 `OpFailed`（`lifecycle.go:60`）—— 此时**没有任何东西会再试它**。而 A2 的判据只看 `StartAttempts`，于是把这种死角报成「正在恢复」。这正是「控制面以为有东西在路上」的谎言。已改为：LOST 行的重试判据看**准备预算**（`PrepareAttempts < PrepareMaxAttempts`），烧完即 `Stranded`，于是 planner 会去补那个洞、告警会指名要人处理。
- ⛔ **刻意不做**：**不**自动释放「还列在伙伴清单里但不应答」的容器。`markLost` 的契约写得很清楚 —— 沉默不证明死亡；自动释放等于把一台可能活着的 8 卡机器还回去。能自动的只有「平台自己说它没了」这一条。平台侧的重启原语（B 那一半）在 k8s 上有（liveness），在九章/SSH 伙伴上**没有**。

---

## 7. 验证

```bash
go build ./... && go vet ./... && gofmt -l .
go test ./... -count=1          # 16 个包全绿
cd bootstrap && python -m unittest discover -s tests -t .   # 77 项通过 / 2 跳过
git diff --check                # 无空白错误
```

新增的**主要**断言（不是全部）：

- B1：持服务锁时 liveness 必须 200 / health 必须未完成（双向）；`PHASE_FAILED` 时 health 503 而 liveness 200；缺 token 401；版本与 uptime。
- B2：默认不下发探针；开启后 path/port/header/时序与常量一致；**探针路径必须与 readiness 不同**；序列化后的 Pod JSON 里 `livenessProbe` 存在 / 不存在；时序上界。
- A1：默认冷却为 0 时停摆保持不变；冷却内不重臂、不移动 park 时间戳；冷却后 `attempts=0 / rearms=1 / park 清空 / 审计 `service_start_rearmed``；**不推进时钟就不能二次重臂**；重臂后能真的恢复 SERVING 并清零债务；上限用满后无论过多久都不再重臂；操作者 requeue 同样清零债务；只有烧完预算才打 park 时间戳。存储层另加 v3→v4 迁移与「park 时间戳往返」。
- A2/A3：停摆（无可重臂）报 `Stranded`、可重臂报 `Recovering`、LOST 报 `Recovering` 且**不计入 serving**；`Stranded` 不抑制 `RELEASE`；快照撤销把 LOST 行清成 `RELEASED`；对哑容器零 `stop` 调用。

---

## 8. 要让它在线上生效，还需要做什么

| 目标 | 动作 | 是否已做 |
| --- | --- | --- |
| A2 / A3 | 重建控制面镜像 → 按 `ns-deploy-cp.py` 滚动部署（含 `--check-config` 预检 + `imageID` 逐字校验） | ❌ 待做（代码就绪） |
| A1 | 在上面那份配置里加 `start_retry_cooldown: 30m` + `start_retry_max_rearms: 3`（配置**只在启动时读一次**） | ❌ 待做 |
| B（nightshift） | ① 用本仓 `bootstrap/` 重建 worker 镜像并推 ecis；② 把 `kubernetes.image` 换到新 tag；③ 置 `kubernetes.liveness_probe: true`；④ 重建 worker Pod（一次 drain + 约 7 分钟重载） | ❌ 待做（镜像重建是前置） |
| B（九章） | 同 ①；但九章侧没有容器重启原语 —— liveness 是 k8s 的机制 | n/a |

**注意 rollback 语义**：迁移只 **加列**，回滚到旧二进制不会坏（旧代码不读这两列）。但一旦 `liveness_probe` 打开且镜像 tag 回退，容器会进 CrashLoop —— 这两个值必须一起回退。

---

## 9. 仍未闭环

1. **九章 / SSH 伙伴没有容器级重启原语**。worker 冻住时 B 的原理（kubelet 看探针）不存在，能自愈的只有 SGLang 自带的进程内 watchdog（默认 300s，且**不覆盖权重加载期**）。要让那一侧也自愈，需要伙伴侧提供一个 stop/start 原语，或让控制面用 SSH 重新拉起 entrypoint（这是一条新链路，不在本轮范围）。
2. **B 的真机验证缺口**：代码、单测、配置前置条件都就绪，但 nightshift 上跑着的还是 v0.2 worker 镜像，**没有** `/bootstrap/liveness`。本轮**没有**把探针打开，也没有重建镜像 —— 所以「kubelet 真的会重启冻住的容器」这条结论，目前只有**单元级 + 机制级**证据，没有真机证据。
3. **`bootstrap_version` 仍是 `tai-talea-bootstrap/1`**。加了端点其实是契约变化，严格说应升到 `/2` 并用 `image_profile.bootstrap_version` 的握手把「旧镜像 + 新探针」这种组合变成**显式错误**而不是 CrashLoop。没有现在做，是因为它牵动 `deploy/container/profile.json`、helm configmap、文档与**正在运行**的部署（v0.6 控制面编译进去的是 `/1`，一升就会在下次 prepare 时拒掉现役 worker）。正确顺序是：重建镜像时一并升级，与控制面一起滚动。
