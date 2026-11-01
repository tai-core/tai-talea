# AlayaNeW 容量与配额探测（2026-10-26）

结论先行：**这个账号的配额单位是「实例个数」，上限 3。** 16 卡不是配额，是「2 个 8 卡实例」的结果。
现用 2 个实例，**只能再开 1 个**。GPU 弹性资源取不到卡，扩容只能走 `EXCLUSIVE`。

## 1. 为什么之前答不出这个问题

`remainingCount`（`GET /api/osm/v1/product/list-for-cci?aidcId=2`）看起来像配额，其实是
**平台资源池余量的异步快照**。三条否证：

| 观察 | 结果 |
| --- | --- |
| 下单后（实例 `QUEUED`） | 数字不变 |
| 实例真落成 `RUNNING` 后 | 数字仍然不变（CPU 实例实测） |
| 21:16 → 21:29 不操作本租户 | 69/34/17/8 → 61/30/15/7，四档**同步各减 8 卡** |

第三条同时证明：1/2/4/8 卡四档是**同一个物理池**按粒度折算（69 卡 ≈ 34×2 ≈ 17×4 ≈ 8×8），
不是四个独立库存。数字的变动来自**别的租户**消费。

配额相关的 17 个路径（`order/list`、`quota/*`、`tenant/*`、`account/balance`、`aidc/list` …）
全部 `404 Not Found Api`，文档也写明配额只在控制台「客户中心 / 权限管理 / 资源配额」。
所以只剩一条路：**让平台自己拒绝，然后读它的错误码。**

## 2. 探测方法

脚本 `tmp/alayanew/cci-elastic-probe.py`：下单 → 每 5 s 轮询状态 → `finally` 里必定 release。

两条安全约束是刻意设计的：

- **全程不调 `open-port`。** ~~于是 9001 不发布，控制面 pull 会跳过它~~
  **（2026-10-27 复核：这条安全假设不成立，见下方更正）。**
- **前后各 sweep 一次**（`tmp/alayanew/cci-probe-sweep.py`）。原因是第一次探测被 shell 默认
  超时 SIGTERM 杀掉，`finally` 没执行，留下一个 `QUEUED` 孤儿——它随后把下一次下单挡成了
  「配额已满」，差点把结论带偏。

> **2026-10-27 更正**：昨天写「不开 `open-port` ⇒ 9001 不发布 ⇒ 控制面 pull 会跳过该实例」
> 是错的。`deploy/alayanew.example.yaml` 的注释写明 **CCI 默认就发布容器端口 9001 与 9002**
> （各映射到一个无关公网端口），不需要我们调用 `open-port`。
> 所以探测实例真正没有被纳管的原因是：**它们几乎都没活到 `RUNNING`**——
> 只有 CPU 那台到过 `RUNNING`，且 22 秒后即被释放，没赶上 60 秒一轮的 pull。
> 今后同类探测**不能**再拿「没开端口」当安全保障；要么缩短窗口，要么先确认不会被 pull 看到。

## 3. 三组结果

| # | 规格 | 结果 |
| --- | --- | --- |
| A | 8 卡 **ELASTIC**，prio 3 | 接受 → 实例 `QUEUED`，**300 s 未落卡** → release |
| B | `云容器实例-CPU` ELASTIC | **22 秒落卡**（21:22:46 STARTING → 21:23:07 RUNNING）→ release |
| C | 1 卡 **ELASTIC**，prio 3 | 干净重做后 `QUEUED` **150 s 未落卡** → release |

B 与 A/C 的对比说明：**弹性通道本身是通的**，CPU 22 秒就能拿到。
问题不在「排队策略」，而在**GPU 弹性池取不到卡**——1 卡和 8 卡都一样，
所以也不是「整节点不够」这种粒度问题。

## 4. 真正挖到配额的那次失败

1 卡探测第一次跑时并没有测到 GPU，而是撞上了配额：

```json
{"status": "FAILED", "resources": [{
  "skuCode": "skus.cci.gpu.h100.80gb.hbm3.b11.a2.5.11.saa70bd",
  "errorCode": 40010,
  "errorMessage": "您的剩余配额为：0，已达上限：3，请等待资源可用或联系管理员调整资源分配。"
}]}
```

那一刻实例数是 **3**（pd2 + pd3 + 那个 `QUEUED` 孤儿），卡数是 16 + 1 = 17。

排除法：`上限：3` 若是卡数该显示 16 或 17；若是 8 卡单元该是 2.x（17/8）。
只有「**实例个数 = 3**」对得上。所以：

- **配额 = 3 个实例，现用 2，剩 1。**
- **`QUEUED` 的实例也算数**——孤儿排队 8 分钟，就把第三次下单挡成 40010。

## 5. 顺带发现：自动扩容有两个断点

既然要谈「弹性扩容」，必须记下这两条，否则补了 onboarding 也白补。

### 5.1 `WaitCreation` 用的接口已停用

```
GET /api/osm/v1/subscription/CCI/result/{key}
  → {"status":212147,"message":"该接口已停用，请改用 /v1/subscription/sku/cci/result/{resultKey}"}

GET /api/osm/v1/subscription/sku/cci/result/{key}
  → 200 {"data":{"status":"FAILED","resources":[{"errorCode":40010,...}]}}
```

`internal/partners/alayanew/client.go:237` 的 `subscriptionResultOnce` 仍在用旧路径。
key 含冒号，必须**原样**放进路径（URL 编码会让签名不匹配）。

### 5.2 订阅 SUCCESS ≠ 实例 RUNNING（2026-10-27 复核后修正）

8 卡弹性那单的订阅结果 `data.status = "SUCCESS"`，还给了 `resourceInstanceId`，
可实例始终 `QUEUED`。所以 `WaitCreation` 会把「订上了」当成「建好了」返回 instanceId。

**但「接着 `AwaitSSHAccess` 白等 30 分钟」这句昨天写错了。** 复核
`adapter.go:203 awaitingCapacity`：`QUEUED` / `PENDING` / `SCHEDULING` 会被识别为
「平台还没给我们容器」，此时 `deadline` 被清零、**不计时**，`seenContainer` 也不置位。
等到 `provisionTimeout`（默认 45 分钟）到期时，它返回 `ErrAwaitingCapacity`，
`runProvision` 据此把任务 **hold 成 `QUEUED`**，**不释放、不失败**——保留队列位次是刻意设计
（`adapter.go:215` 的注释：排队时没有容器、不计费，本地超时唯一能干的事就是把自己排到队尾）。

真正的影响是另一条：`OutstandingProvisions()` 只把 `MANAGED` / `FAILED` 算作终结，
所以这种挂住的 `QUEUED` 任务会一直计入 `Input.Capacity.Provisioning`，
而 `capacityStep` 在 `Provisioning > 0` 时**不再下任何新单**。
**一个永不落卡的 GPU 弹性单，会把后续所有自动扩容永久堵死。** 这比「白等 30 分钟」严重。

三者叠加，当前的自动扩容路径（`planner → RequestCapacity → CreateInstance →
WaitCreation → AwaitSSHAccess`）实际会在三个地方断：

1. `onboarding_enabled: false` → `503 errProvisionNoOnboarding`
2. `WaitCreation` → `212147 接口已停用`
3. `WaitCreation` 误判 SUCCESS 语义 → 白等 30 分钟

## 6. 对「能不能扩到 24 卡」的回答

- **能，但只有一次机会，而且必须走保障资源。** 实例配额剩 1；这一台用 `EXCLUSIVE`
  才有机会落卡（GPU 弹性实测拿不到）。落下来就是 3 × 8 = 24 卡。
- **如果这一台也开 8 卡并占了 `EXCLUSIVE`，之后就再没有余量了**（配额 3 用满）。
  要再扩必须让平台调配额。
- **不能靠弹性扩容自动长**：`planner.mode: fixed` + `capacity_actions: false`，
  且线上 partner 配置写死 `compute_resource: EXCLUSIVE`——真开也只会下保障资源单，
  不会去抢弹性池。

## 7. 现场状态

探测后实例列表只剩 `glm53-pd2` / `glm53-pd3`（均 `RUNNING` / `EXCLUSIVE`）；
Router `prefill` + `decode` 均 `healthy=True`，URL 未变；`9*9` → `81`，`finish=stop`。
线上服务全程未受影响。

## 8. 待办（未执行）

1. **`EXCLUSIVE` 8 卡验证**：唯一还没证的一件事——保障资源套餐是否允许第 3 台。
   成功即 24 卡，失败即回 `40010`。有计费影响，需先确认。
2. 修 `subscriptionResultOnce` 到新路径（否则自动扩容永远走不到终点）。
3. 补 `onboarding.script`（当前 `onboarding_enabled: false`）。
4. 之后才谈 `planner.mode: auto`。
