# Talea 设计架构说明书

本文是 tai-talea 的设计与架构总纲：系统解决什么问题、由哪些组件构成、核心状态机与
生命周期如何运转、自愈体系的两条手臂、以及配置/存储/部署的工程约定。

- 日常操作（看状态、下单、救火、排障）见 [中国移动平台工具使用说明书](cmcc-operations-manual-2026-10-15.md)。
- 网页端使用（上线、回收、监控、压测）见 [使用说明书](user-guide.md)。
- 简版项目说明（构建、首次部署、接口约定）见 [项目说明书](project-guide.md)。
- 本文只讲**设计与架构**，不含操作手册内容，也不含任何凭据。

## 1. 项目定位

tai-talea 是 GPU 推理容量的**控制面**：管理合作商（partner）提供的 GPU 容器，
在节点内启动 SGLang Prefill/Decode 服务，并通过 sglang-router 动态接流、摘流与回收。

问题域的三个硬约束决定了全部设计：

1. **容量是别人的**。容器由云平台创建与销毁，控制面只能观察与适应
   （pull 快照 + push 事件），不能假设自己独占节点。
2. **PD 分离架构下角色是稀缺资源**。Prefill 与 Decode 必须成比例存在
   （固定档位如 1:1），单机即整节点（一台 worker 吃 8 卡），
   少一台就退化为零服务。
3. **故障形态恶劣**。GPU 加载期会 CUDA OOM（静默自杀），运行期会 NVRM 卡死
   （进程活着但完全哑，SIGTERM 无效）。控制面必须区分"死了"和"冻了"。

## 2. 总体架构

```
        ┌─────────────────────── 控制面（Go 单体，tai-talea）───────────────────────┐
        │                                                                          │
  push 事件   ┌─────────┐   ┌────────────┐   ┌──────────┐   ┌─────────────────┐     │
 (partner)→──→│  api    │──→│ controller │──→│ instance │──→│ store (SQLite)  │     │
              │ console │   │ (reconcile)│   │ manager  │   │ 事件幂等+状态机  │     │
  HTTP 页面←──→│ onboard │   └─────┬──────┘   └────┬─────┘   └─────────────────┘     │
              │provision│         │               │                                 │
              └─────────┘         ▼               ▼                                 │
                          ┌──────────────┐  ┌─────────────┐                         │
                          │ planner      │  │ partner     │                         │
                          │ (P:D 档位)   │  │ adapters    │                         │
                          └──────┬───────┘  └──────┬──────┘                         │
                                 │                 │ pull/provision/release          │
                          ┌──────▼───────┐         ▼                                 │
                          │routeradapter │   ┌──────────┐        ┌──────────────┐    │
                          └──────┬───────┘   │ 云平台   │        │ telemetry    │    │
                                 │           │ (k8s/CCI)│        │ (负载五门)   │    │
        └────────────────────────┼───────────┴──────────┴────────┴──────────────┴────┘
                                 │ REST (workers 注册/摘流/删除)
                                 ▼
                      ┌────────────────────┐
                      │  sglang-router     │ ←—— Rust，独立仓库 tai-core/sglang-router
                      │  (PD 分离路由)     │
                      └─────────┬──────────┘
                注册/心跳/摘流   │   推理流量
        ┌───────────────────────┼────────────────────────┐
        ▼                       ▼                        ▼
  ┌───────────┐          ┌───────────┐            ┌───────────┐
  │ worker Pod│          │ worker Pod│            │ worker Pod│  每个容器内：
  │ (prefill) │          │ (decode)  │            │  …        │  bootstrap(Python)
  │ bootstrap │          │ bootstrap │            │           │  → SGLang serve
  │ → SGLang  │          │ → SGLang  │            │           │  → 向 router 注册
  └───────────┘          └───────────┘            └───────────┘
```

四个进程级组件：

| 组件 | 语言 | 仓库位置 | 职责 |
|---|---|---|---|
| 控制面 `tai-talea` | Go | 本仓库 `cmd/tai-talea` + `internal/*` | 状态机、生命周期、计划、partner 适配、HTTP API |
| worker bootstrap | Python（仅 stdlib） | 本仓库 `bootstrap/tai_talea_bootstrap` | 容器内启动/停止/探活 SGLang，参数白名单 |
| sglang-router | Rust（`smg`） | 独立仓库 `sglang-router` | PD 分离路由、worker 注册表、摘流 |
| SGLang 运行时 | Python | 社区版 0.5.20（烤进 worker 镜像） | 模型推理本身 |

控制面是单进程单体，故意不拆微服务：状态强一致（单写者 + SQLite），
故障域小（重启即全量恢复），部署极简（一个二进制 + 一个配置文件）。

## 3. 控制面内部结构（`internal/`）

| 包 | 职责 | 关键约束 |
|---|---|---|
| `domain` | 双轴状态机、事件、转换校验 | `CheckCombination` 判定合法组合；`ValidateTransition` 拒绝非法跳转 |
| `store` | SQLite 持久化：实例、事件、租约 | 事件按 `event_id` 幂等去重；schema 迁移只加列（v4） |
| `instancemanager` | 容器级准备（Prepare）：镜像指纹、内网地址、`bootstrap_version` 配对 | 唯一版本校验点在 `manager.go` 的 `Prepare()` |
| `controller` | reconcile 主循环：把实际世界推向期望状态 | 所有副作用走这里；异步工作流用独立 timeout（`removalContext`） |
| `planner` | P/D 角色规划：固定档位、冷却、下限保护 | 只产出 `ActionKind`，不 import controller（防循环依赖） |
| `partner` + `partners/` | 云平台抽象：`registry.go` 注册表，`alayanew`（CCI）、`kubernetes`（cmcc）两实现 | 不引 client-go（用原生 API）；接口可选 `InstanceIdentity` |
| `routeradapter` | Router 的 REST 客户端：注册/摘流/删除/成员等待 | 删除是异步的，必须轮询等待落地 |
| `api` | HTTP 服务：console（页面+操作）、push（partner 事件）、onboarding、provision | Bearer 鉴权；UI 是内嵌静态页 |
| `telemetry` | 负载采集：五门可用性判定 | 只认 `/v1/capacity/console/telemetry` |
| `launcher` / `onboarding` / `obs` / `config` / `buildinfo` | 服务启停编排、SSH 上线、指标、配置加载、版本信息 | 配置启动时读一次，`KnownFields(true)` 拒未知字段 |

依赖方向单向：`api → controller → {instancemanager, planner, partner, routeradapter, store}`。
`planner` 不知道 controller 的存在（经 `Deps.Capacity` 注入拿数据），这是刻意的。

## 4. 核心设计：双轴状态机

每个实例（纳管的一台容器）有**两个正交状态轴**，见 `internal/domain/state.go`：

- **InstanceState（容器轴）**：`ALLOCATING → PREPARING → IDLE → RELEASING → RELEASED`，
  外加异常态 `LOST`（云平台侧消失/不可达）。
- **ServiceState（服务轴）**：`NONE → STARTING → HEALTHY → REGISTERING → SERVING →
  DRAINING → (NONE)`，外加 `FAILED`。

合法性由 `CheckCombination(instance, service, role)` 判定——不是任意两轴组合都合法
（例如 `RELEASED` 上不允许有服务，`SERVING` 必须挂在 `IDLE` 上）。
转换合法性由 `ValidateTransition` 校验，非法跳转被拒并告警（`SeverityReject`）。

设计要点：

- **两轴分离的原因**：容器在不在、服务起没起，是两件独立的事。容器可能被平台
  杀掉但服务记录还在（要清理），也可能容器健康但服务反复起不来（要烧重试预算）。
- **持久化即真相**：每次转换先落库再执行副作用，控制面重启后从 SQLite 全量恢复
  （`TestAcceptanceReconcileRecoversAfterRestart`）。
- **事件幂等**：partner push 事件按 `event_id` 去重（`ClaimEvent`），重复投递不产生
  第二次生命周期动作（`TestClaimEventIsIdempotent`）。

## 5. 生命周期主流程

```
下单 provision → 建容器 ALLOCATING → Prepare（指纹/地址/版本配对）PREPARING
→ 纳管 enroll → IDLE（容器就绪，无服务）
→ start：bootstrap 拉起 SGLang → STARTING →（健康检查）HEALTHY
→ 向 router 注册 → REGISTERING → router 判健康 → SERVING
→ 摘流 drain：PUT readiness unavailable → DRAINING → 等在途请求 → 停服务 → NONE
→ 归还 release → RELEASING → 平台删容器 → RELEASED
```

关键工程事实：

- **Prepare 的三重身份一致**：spec / profile / image_profile 三处基线不一致直接
  exit 29（快速失败，不带病上岗）。
- **内网地址的唯一来源是"装的时候容器内实测"**（`EnrollPrepared` →
  `adoptVerifiedEndpoint`），因为平台只发布 HTTP ingress，PD 握手端口不发布；
  不能从平台快照猜。
- **摘流唯一原语是 `PUT /state/workers/{id}`**（readiness 不可用）。Router 的
  `DELETE /workers/{id}` 只用于成员清理，正常摘流永远走 readiness，保证在途请求
  优雅完成。
- **释放的前置是 `ServiceState==NONE`**：先摘流停服，才允许归还容器。

## 6. Partner 适配层

`internal/partners/registry.go` 定义注册表，两个生产实现：

| 实现 | 平台 | 模式 |
|---|---|---|
| `partners/kubernetes` | 中国移动 nightshift（ecis/k8s） | **SelfProvisioningAdapter**：`ProvisionNode(name)` 直接建 Pod（镜像已烤 bootstrap + SGLang），不需 SSH |
| `partners/alayanew` | 九章智算云 AlayaNeW（CCI） | 平台 API（HMAC 签名），pull 快照 + push 事件 |

适配层的核心约定：

- **纳管身份 = partner 快照的 key**（CCI 是容器名，不是平台 UUID）。用 UUID 会导致
  同一物理箱出现两条记录 + 重复调度。可选接口 `partner.InstanceIdentity` 让平台
  显式提供稳定身份。
- **`partner.ErrInstanceNotFound` 必须当成功处理**：释放一个不存在的实例说明目标
  已达成，否则实例永远卡在 RELEASING 占配额。
- **pull 与 push 互补**：push 是快路径（事件驱动），pull 是兜底（对账修复漂移），
  默认 pull 只取 RUNNING。

## 7. Router 适配层契约

Router（Rust，独立仓库）与控制面之间有四条硬契约，全部来自真机事故：

1. **健康节奏契约**：Router 默认健康检查 60s×2，新 worker 最坏 120s 才判 healthy，
   而控制面 `ensureReadiness` 只等 25s——已成功的启动会被稳定判死。Router 部署
   必须带 `--health-check-interval-secs=5 -timeout-secs=3 -success-threshold=1`。
2. **不能原地改角色**：`PUT` 不改 `worker_type`，换角色只能 delete+add；
   且删除是异步工作流（返回 202 时条目还在），适配层必须轮询等待真正落地。
3. **异步工作流不能借 `call_timeout`**：删除用独立的 `removal_timeout`（8m）+
   `Controller.removalContext`，否则 10s 调用超时会误杀正常的慢删除。
4. **worker id 是进程内随机 UUID**，`url_to_id` 映射在 Router 内存里：Router 重启
   后 id 全换；同 URL 重注册复用 id（drain 状态会粘住）。控制面不能假设 id 稳定。

## 8. Planner（P/D 比例规划）

`internal/planner/planner.go`，职责：在 fixed 模式下维持 `gear`（如 `"1:1"`）档位。

- **计数口径**：`total = Serving + Idle`；`TargetPrefill = floor(total × ratio)`；
  仅当 `total >= 2` 才把目标夹到 `[1, total-1]`。计划数按 Serving 侧算。
- **下限保护**：永不拿走某角色最后一个 worker（`TestPlanNeverEmptiesARole`）——
  0P1D 不是降级而是零服务，这条保护是硬性的。
- **防重下单**：排队中的容器对 planner 不可见，靠 `Input.Capacity.Provisioning`
  显式计入在途单，避免每轮 reconcile 重复下单。
- **已知边界**：会把 1-of-2 的降级放大成全停（实测 4m14s 收敛）；实时自适应
  P/D 未实现（`capacity_actions: false` 时只读不算）。

机器级 SGLang 参数三层合并（后写覆盖前写）：
`extra_sglang_args` → `by_role` → `nodes[]`，最终进 `InstanceSpec.SGLangArgsOverride`。

## 9. 自愈体系：A（重试预算）+ B（liveness 合约）

### A：启动重试预算与"重臂"（v0.7 起现役）

针对加载期 CUDA OOM（静默自杀、可重试）：

- 启动预算 `start_max_attempts: 3`；烧完后进入 `Recovering`（只读桶），
  **不计入 planner 的 total**——计入会让容量缺口被"转换健康 worker"错误填掉。
- 冷却时钟只用专用列 `start_parked_at`（`LastSeenAt` 会被 pull 重申刷新，永不到期）。
- 冷却到期后"重臂"复位：attempts 清零、rearms+1（上限 `start_retry_max_rearms`）、
  OpStart 重写为 Pending。
- **手动复位**：`POST /v1/capacity/console/instances/{id}/requeue` 复位整个预算
  （前置：`IDLE/FAILED` 且预算已烧完且有角色）。A1 用尽的实例永远可救，
  不必滚控制面。

### B：liveness 探针合约（v0.3 worker / v0.8 控制面，已上线）

针对 NVRM 卡死（进程活着但完全哑，SIGTERM 无效）：

- 新端点 `/bootstrap/liveness`：**不持锁、不碰模型、不碰磁盘**——它与"健康"
  （`health` 持锁 + 问模型）和"卡死"三种状态在旧端点上长得一样，必须分开。
- kubelet livenessProbe 打在 Pod spec 里（创建时固定），必须带 `X-Bootstrap-Token`，
  时序写死 60/10/5/6；冻住 ⇒ 探针连失 ⇒ kubelet SIGKILL（exitCode 137）⇒
  同 Pod 同 IP 原地重启。SIGKILL 不可捕获，是唯一能结束冻住进程树的手段。
  该主张已真机验证（SIGSTOP 模拟冻死，restartCount 0→1→2，每次 137）。
- **配对防线 `bootstrap_version`**：编译期常量（控制面与 worker 镜像各自烤入），
  唯一校验点在 `instancemanager` 的 `Prepare()`。版本不配（如控制面 `/2` 对
  容器 `/1`）在**准备期大声失败**，而不是探针期静默 CrashLoop。
- **窗口约束**：探针在 Pod spec 创建时固定，所以滚控制面必须与"提升配置 +
  重建全部 worker Pod"同窗口，且控制面先于任何 worker Pod 创建升到新版本。

### 一条反模式

**删 Pod + 同名重建不可用**（实测）：pull 把 Pod 消失读成容量被平台回收 ⇒
自动 drain→release（真删 Pod）；同名再 enroll 撞 Released 报并发冲突；
provision 的 FAILED 记录在内存里，同名再下单永远 409。
**可用路径 = 删 Pod + 用新名字 provision**。

## 10. 配置系统

- **启动时读一次**（`--check-config` 只做预检不起服务）；`KnownFields(true)`，
  未知字段直接拒——配置漂移在启动期暴露，不在运行期。
- `push_secret` / `hmac_tolerance` 必填（无默认值的鉴权字段不允许隐式为空）。
- **镜像与配置必须一起回退**：旧二进制不认识新字段（如 `start_retry_*`），
  只回退镜像 tag 会启动失败；`bootstrap_version` 的 `/1`↔`/2` 同理。
- **候选配置走侧路径**：先写 `*.next.yaml` → 新镜像 `--check-config` →
  通过才覆盖 live 配置（先覆盖会留下"旧 Pod 一重启就起不来"的窗口）。
- 仓库内的 `deploy/*.yaml` 全部是**模板**（`change-me-*` 占位符），
  线上配置只存在于集群节点上；凭据绝不入库。

## 11. 存储与恢复

- SQLite（`modernc.org/sqlite`，纯 Go 无 CGO）单文件，控制面唯一状态库。
- schema 迁移**只加列**（当前 v4 加了 `start_parked_at`）：旧二进制回滚不会被
  新列卡死。
- 事件表按 `event_id` 主键幂等；实例转换全部经 `store.ApplyTransition`，
  持久化先于副作用。

## 12. API 面与鉴权

| 面 | 路径前缀 | 用途 | 鉴权 |
|---|---|---|---|
| console | `/v1/capacity/console/*` | 页面 + 操作（实例/服务/requeue/planner/telemetry） | `admin_token`（Bearer） |
| push | partner push 回调 | 容量事件（HMAC 签名 + 时间窗） | `push_secret` + `hmac_tolerance` |
| onboarding | `/v1/onboarding/*` | SSH 上线（SFTP 分发运行时） | `admin_token` |
| provision | `/v1/capacity/console/provision*` | 自助下单（SelfProvisioning 平台） | `admin_token` |
| UI | `/` | 内嵌静态页（`internal/api/ui`） | 同 console |

worker 侧 bootstrap 的四个 `/bootstrap/*` 端点共享另一个密钥
（`X-Bootstrap-Token`，来源 `TAI_TALEA_BOOTSTRAP_TOKEN` 或 `--token-file`），
常量时间比较（`hmac.compare_digest`），无密钥拒绝启动。

## 13. Worker bootstrap（`bootstrap/tai_talea_bootstrap`，纯 stdlib）

| 模块 | 职责 |
|---|---|
| `server.py` | HTTP 服务与共享密钥校验（`/bootstrap/start|stop|status|health|liveness`） |
| `service.py` | SGLang 进程树的启动/停止/探活 |
| `roles.py` | Prefill/Decode 角色参数装配 + **参数白名单** |
| `environment.py` / `profile.py` | 容器内环境实测（内网地址、GPU）与基线指纹 |
| `exitcodes.py` | 语义化退出码（如 29 = 身份不一致、35 = 未配置密钥） |

参数白名单是安全边界：控制面下发的 `extra_args` 逐 token 校验，
`--disaggregation-mode` 等角色参数由 bootstrap 自己注入，写进 extra_args 会被 422。

## 14. 部署形态

### 中国移动 nightshift（现役主力）

- k8s 集群，ns `nightshift`；h100-01 / h100-04 各 8×H100；一台 worker 吃整节点
  （`gpu_count: 8`）⇒ 1P1D 需要两台节点都空。
- 控制面与 Router 都是 Pod（hostNetwork 钉在 h100-04）；worker Pod 由控制面经
  k8s API 自建（SelfProvisioning）。
- 部署链：本机交叉编译（`CGO_ENABLED=0` + buildinfo 注入版本/commit/时间）→
  纯 stdlib 脚本打 OCI 镜像并 push → preflight → `ns-deploy-cp.py` 按 digest 部署
  （imageID 逐字比对，tag 不是证据）→ 版本自证。
- 版本线：v0.6 → v0.7（自愈 A）→ **v0.8 现役**（自愈 B，`bootstrap_version /2`）。

### 九章智算云 AlayaNeW（CCI）

- 平台 API + HMAC；只有 `release` 没有 `start`/`stop`（停 ≠ 释放）；
  实例名一次性（重用报 212035）；Router 需 `--pd-disaggregation` 才可路由 PD worker。

## 15. 可观测性与取证

- **负载只认** `GET /v1/capacity/console/telemetry`；可用性门是 Running && Queue &&
  MaxRunning && Prealloc && Transfer 五项全非空，缺一项则该 worker 的负载输入
  静默关闭（`Complete=false`）。
- 加载期死法取证只看 worker 容器内 `/opt/tai-talea/logs/sglang-*.log`
  （bootstrap 的 stdout 只有 HTTP 访问行，看不到 SGLang 死因）。
- 控制面自身暴露 metrics；watch 观察者（`ns-watch.py`）提供跨会话整段状态历史。

## 16. 测试与质量基线

- 17 个包测试全绿：`go build ./... && go vet ./... && go test ./...`
  （controller 集成测试约 80s，为最长包）。
- 验收测试贯穿设计：`TestAcceptance*` 系列直接对应生命周期承诺（幂等、非法转换、
  readiness 摘流、重启恢复、档位执行）；每条新契约落地都先有 failing test。
- bootstrap 侧有独立 Python 测试（`bootstrap/tests/test_bootstrap.py`），
  覆盖白名单注入攻击（如 `--model-path=/etc/passwd`、`; rm -rf /`）。

## 17. 设计原则汇总

1. **持久化先于副作用**，重启即恢复。
2. **事件幂等**，重复投递无害。
3. **身份三重一致**，不一致快速失败（exit 29）。
4. **版本配对大声失败**，不在探针期静默 CrashLoop。
5. **摘流只走 readiness**，在途请求优先。
6. **目标已达成即成功**（`ErrInstanceNotFound`），不卡终态。
7. **下限保护硬性**：永不拿走最后一个角色。
8. **凭据绝不入库**：仓库公开，配置模板化，凭据只存在于集群节点与环境变量。
9. **tag 不是证据**：部署与回滚一律按 image digest 逐字比对。
10. **同名不重建**：实例名一次性，重建必换名。

## 18. 文档地图

| 文档 | 内容 |
|---|---|
| [user-guide.md](user-guide.md) | 网页端使用：上线、回收、监控、压测 |
| [project-guide.md](project-guide.md) | 简版项目说明：构建、首次部署、接口约定 |
| [cmcc-operations-manual-2026-10-15.md](cmcc-operations-manual-2026-10-15.md) | 中国移动平台日常操作与排障手册 |
| [talea-worker-v03-and-the-liveness-contract-2026-10-15.md](talea-worker-v03-and-the-liveness-contract-2026-10-15.md) | B 窗口完整记录与真机证明 |
| [self-healing-liveness-and-start-budget-2026-10-15.md](self-healing-liveness-and-start-budget-2026-10-15.md) | 自愈 A/B 的设计推演 |
| [provisioning-design.md](provisioning-design.md) | 自助下单（SelfProvisioning）设计 |
| [kubernetes-partner.md](kubernetes-partner.md) | k8s partner 适配契约 |
| [M1.md](M1.md) | 里程碑验收矩阵 |
