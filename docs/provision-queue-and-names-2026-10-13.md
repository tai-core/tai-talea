# 按需创建的队列语义与实例名（2026-10-13）

起因：回到九章云做只读侦察时，发现**推理完全不可用**（`503 No available prefill workers`），
并顺着 provision 作业列表查出两条**我们自己的**失败链。两处都已修，本文记录证据与语义。

---

## 0. 结论速览

| # | 事实（实测） | 处置 |
| --- | --- | --- |
| 1 | `glm53-auto5` 排队 **5h30m 后被我们自己释放**（`createTime 19:57:16` → `releaseTime 01:27:33`，正好等于 `wait_timeout`） | 排队阶段不再受 `wait_timeout` 约束；预算到期**不释放**队列位次，作业记为「排队中」而非 FAILED |
| 2 | `glm53-auto4` 硬失败于 **`status 212035 本企业内有重复实例名称`** | 适配器把它映射为 `partner.ErrNameInUse`；provision 层**自动换名重试**（`-2`…`-5`） |
| 3 | 只读客户端把前缀写成 `/api/osm`，返回 `404 {"message":"Invalid AccessKey"}` | 前缀是 `/api/osm/v1`；代码注释与 `docs/alayanew-cci.md` 都已改正 |

---

## 1. 证据链

### 1.1 排队中的请求被我们自己释放了

九章的 provision 作业记录（控制面 `/v1/capacity/console/provision`）：

```
glm53-auto4 | FAILED | 提交平台创建请求 | err= create instance glm53-auto4:
             alayanew api status 212035: 本企业内有重复实例名称，请更换
glm53-auto5 | FAILED | 等待容器 RUNNING 并获取连接信息 | inst= 5dcea1a4-…
             err= instance 5dcea1a4-… not RUNNING within 5h30m0s (last: QUEUED)
```

平台侧同一个实例（`GET /api/osm/v1/cci/instance/5dcea1a4-…`）：

| 字段 | 值 |
| --- | --- |
| `status` | `RELEASED` |
| `priority` | `1` |
| `computeResourceType` | `EXCLUSIVE` |
| `queueTimeoutCancelEnable` | `false` |
| `createTime` | `2026-10-12 19:57:16` |
| `releaseTime` | `2026-10-13 01:27:33` |

`01:27:33 − 19:57:16 = 5h30m`，与配置的 `alayanew.wait_timeout: 5h30m` 逐字相同。
**平台不会自动取消排队**（`queueTimeoutCancelEnable: false`、`queueTimeoutHours: null`），
所以这个释放只可能来自我们：`AwaitSSHAccess` 到期 → `runProvision` 的 `abandon()` → `POST /cci/instance/{uuid}/release`。

### 1.2 为什么这是错的

- **排队期间没有容器**，因此不计费。配置里那句注释本来就是对的：
  *"Six hours costs nothing while queued — no container exists yet"*。
- 排队的**唯一资产是队列位次**，而 `priority: 1` 是我们专门要来的（平台默认 3）。
  释放 = **从队尾重新排**，等价于把「还在排队」变成「还没开始排」。
- 更糟的是它**伪装成失败**：作业显示 FAILED，运维看到的是「创建失败」，
  看不到「我们本来排在第 1 位」。

也就是说：**放弃队列位次必须是一个显式决定，不能是本地超时的副作用。**

---

## 2. 改后的语义

引入 `partner.ErrAwaitingCapacity`，把「等容量」从「失败」里分出来。阶段与预算：

| 实例状态 | 有容器吗 | `wait_timeout` 是否生效 | 到期/取消时 |
| --- | --- | --- | --- |
| `QUEUED` / `PENDING` / `SCHEDULING` | 否 | **不生效**（预算停表） | 返回 `ErrAwaitingCapacity`，作业记为 **QUEUED 保持**，**不释放** |
| 其它未知状态 | 不确定 | 生效 | 按失败处理并释放 |
| 容器已存在但起不来（如 `CREATING`） | 是 | 生效 | FAILED + 释放 |
| `RUNNING` 但取不到 SSH | 是 | 生效 | FAILED + 释放 |
| `FAILED` / `ERROR` | — | — | FAILED（释放无副作用） |

两条设计上的取舍，都写在代码注释里：

1. **未知状态不当作「排队」**。未知状态可能描述的是一台**已经存在、正在计费**的容器，
   放了它要花钱，而保住一个队列位次不花钱 —— 所以未知状态保持「到期即释放」的严格行为。
2. **只有确认容器存在过，才允许释放**。`AwaitSSHAccess` 记一个 `seenContainer`：
   在它变 true 之前，连状态读取失败都报 `ErrAwaitingCapacity`（把真实原因一并 wrap 进去，
   不隐藏），于是 caller 不会为一个「不知道存在不存在」的东西去调 release。

作业状态沿用既有取值：**QUEUED 同时表示「刚提交」和「平台还没调度」**，
`ProvisionRecord.State` 的注释已写明。`OutstandingProvisions()` 把 QUEUED 计为未完成 ——
这正好是 planner 侧的「capacity held」，避免排队期间每轮重复下单。

---

## 3. 实例名是一次性的

`status 212035` 的成因比看起来更麻烦：

- 平台**释放后不归还实例名**；
- 且**该名不出现在任何列表接口里**（`instance/list` 不返回已释放的容器）。

所以这个名字**看起来是空的，直到 create 调用拒绝它**。任何「先查再建」的校验都挡不住。

处置：适配器把 212035 映射成 `partner.ErrNameInUse`（保留平台原始文案，运维仍能看到 212035），
命名策略留在 provision 层 —— 换名阶梯 `/名称-2` 到 `/名称-5`（`maxProvisionNameAttempts = 5`）。

| 性质 | 说明 |
| --- | --- |
| 为什么免费 | create 已被拒绝，**还没有分配任何资源**，重试只花一次往返 |
| 为什么有上限 | 名字在平台控制台要认得出来；无上限的阶梯会造出 `glm53-auto4-97` 这种名字 |
| 阶梯被占用时 | `rename` 拒绝覆盖已跟踪的作业（会跳到下一个候选），两个作业共用一个名字会让 tracker 说谎 |
| 作业跟着改名 | `provisionTracker.rename` 把作业键迁到平台接受的名称上，作业列表描述的是**存在的那个容器** |

---

## 4. 测试

| 用例 | 断言 |
| --- | --- |
| `internal/api`: `TestAQueuedCreationIsHeldInsteadOfReleased` | 状态 QUEUED、`released` 为空、stage 说明保留队列位次、仍计为未完成 |
| `internal/api`: `TestAContainerThatNeverComesUpIsStillReleased` | 状态 FAILED 且**确实释放了**（否则这次改动就是把配额泄漏换成省队列位次） |
| `internal/api`: `TestATakenNameIsRetriedUnderAFreshOne` | `glm53-auto4` → `glm53-auto4-2`，旧名不留作业，新作业带上容器 id |
| `internal/api`: `TestTheRenameLadderGivesUp` | 恰好 5 次尝试，最后一次为 FAILED，且**从未释放**（创建从未成功过） |
| `internal/api`: `TestRenameRefusesToShadowAnotherJob` | 改名不得覆盖已跟踪的作业 |
| `alayanew`: `TestCreateInstanceReportsATakenName` | `errors.Is(err, partner.ErrNameInUse)` 且文案里保留 `212035` |
| `alayanew`: `TestAwaitSSHAccessKeepsWaitingWhileTheRequestIsQueued` | `wait_timeout` 设为 1ms 也不放弃，持续轮询直到 ctx 结束，报 `ErrAwaitingCapacity` |
| `alayanew`: `TestAwaitSSHAccessAppliesTheBudgetOnceAContainerExists` | 容器已存在时预算照旧生效，且**不是** `ErrAwaitingCapacity` |
| `alayanew`: `TestUnknownStatusIsNotTreatedAsQueued` | 未知状态保持严格行为 |
| `alayanew`: `TestAwaitSSHAccessReturnsTheLegOnceTheQueueClears` | `QUEUED → RUNNING` 后正常返回 SSH 连接信息 |

配套：适配器新增 `Config.PollInterval`（默认 15s）——轮询节奏是可测的前提，
原先硬编码 15s 让「排队不放弃」这条无法在测试里表达。

---

## 5. 还没做的（明确的边界）

1. **预算用尽之后没人接续排队中的请求。** 现在 `provision_timeout`（九章配 6h）到期后
   作业停在 QUEUED、请求仍留在平台队列里，但**没有 goroutine 再去看它**。
   若它之后真的被调度出来，靠 pull 侧的 `listRunning` 兜底接管 —— 但那条路径没有 SSH 铺装，
   九章上会拿到公网 ingress 地址（PD 握手端口不发布，等于不可用）。
   要做的是「排队续看」：把排队阶段的等待移出 provision 预算，容器真出现时接着走铺装。
2. **Router 里指向已释放容器的 worker 不会自愈。** 实测 `RouterAdapter` 没有删除 worker 的接口，
   `glm53-decode4` 释放后它在 Router 里仍以 `worker_type=prefill, is_healthy=false` 存在，
   让 `/workers` 谎报 `prefill_count: 1`。选路会跳过 unhealthy，所以影响可控（503 依旧正确），
   但视图会误导人。
3. **console 还没有 provision 面板**（待办 #64）：`POST/GET /v1/capacity/console/provision`
   两个接口都在，但 `internal/api/ui/` 里没有任何调用方。所以本次给作业新增的状态
   （QUEUED 保持、改名）目前只有 API 可见。
4. **`WaitCreation` 不会把异步的 212035 映射成 `ErrNameInUse`。** 实测平台是**同步**拒绝的
   （错误出现在「提交平台创建请求」阶段），所以当前够用；若将来变成订阅结果里才报，
   这里需要补一次映射。
