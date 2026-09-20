# tai-talea 代码审查与下一阶段实施计划

> 历史审查基线。后续已实现 SSH 自动上线并修复事件、生命周期、Pull 和 Planner 的故障路径；请以 [2026-09-20 审查记录](review-2026-09-20.md) 和 [项目说明书](project-guide.md) 判断当前状态。

日期：2026-09-18。审查基线：tai-talea `54dcc9c`；Router `70839ad`；内部 SGLang stable `5a26fc1f46a88be023d23c3f8fc896a8555a29e7`。

同日后续进展：已实现并部署管理页面、`talea` 客户端及持久化回收入口，详见 `console-and-client.md`。R1 的未知负载等待、R2 的回收意图保护与停止失败重试已加入修复和回归；新增启动中回收的互斥协调，避免迟到启动。以下问题表仍保留初次审查基线，不能据此把所有问题视为仍未修改，也不能把这些局部修复扩大为全部自动化验收通过。真实 GPU 在途摘流尚未重跑，首次自动铺装仍待实现。

## 1. 需求边界与结论

依据《SGLang容器化实例资源管控工具开发文档》、`.workbuddy/memory`、现有架构与铺装设计，工具应承担以下闭环：

1. 合作商已经创建或提供容器；Push `CAPACITY_ADDED` 或 Pull 发现容量后，我方创建容量记录并纳管。
2. 探测环境、准备运行时、分配 Prefill/Decode 角色、启动服务、确认健康、动态注册 Router。
3. 撤销时先关闭 readiness，等待在途请求结束，再停止服务、归还容量。
4. 持久化事件、实例、操作与审计；控制面重启、网络故障、重复通知后可恢复。
5. Pull 最终成为容量事实的权威来源；Push 是实时加速通道。
6. v1 实现固定比例和手动档位，保留负载输入与滞后机制；自适应调度属于后续阶段。

因此，Push ADD 确实是“创建容量”的入口，但创建的是**我方管理面的容量对象**；是否创建云平台底层容器，属于合作商的供给接口，不应由本工具越界假设。

**用户进一步确认的硬性验收条件：所有运行期自动化由 control 上的 Talea 执行，不依赖 AI agent。** 本轮人工 SSH、依赖排查、制品构建只是开发联调；不能据此声称最终闭环完成。平台给出裸容器和连接信息后，Talea 必须驱动探测、铺装、bootstrap 启动、角色分配、服务注册及后续恢复。新增容量时不得要求开发者逐台进入容器安装。制品可以由 CI/受控构建流程预先发布，运行期只选择已验证的版本，遇到不支持的环境应明确失败并告警。

Router 是独立进程，通常消耗 CPU、内存和网络，不要求独占 GPU 服务器。本轮按用户指定放在 control，与 Talea 共机；P/D 节点仅承载 bootstrap 与推理服务。生产是否独立部署，应依据吞吐、故障隔离与高可用要求决定。

现有代码已经具有完整的 M1 结构和正常路径测试，适合继续完善。**不能把已有测试通过等同于故障语义和内部 SGLang fork 已验收。** 首要工作是明确真实环境兼容性，并修复可能提前停服务、重新启用已撤销容量、丢失事件意图的问题。

## 2. 当前实现与验证基础

| 能力 | 现有实现 | 本次评价 |
|---|---|---|
| Push | Token、时间窗口、HMAC、事件去重、审计 | 正常路径已有；事件落库和执行之间仍有崩溃窗口 |
| 容量/服务状态 | 双层状态、事务校验、operation | 设计方向正确；恢复和失败分支仍有不一致 |
| Bootstrap | 标准库 HTTP、共享密钥、参数白名单、进程监督 | 可作为受控运行入口；裸容器首次铺装需要额外机制 |
| Runtime | 离线 wheelhouse、兼容 profile、venv | 内部 fork 必须固定提交、构建制品、锁依赖 |
| Router | 注册、readiness、generation、负载查询 | 正常路径完整；负载未知、角色变化需要加强 |
| Planner | 固定 1:1 / 2:1 / 1:2、cooldown、min_hold | 需要明确维护/撤销意图优先级 |
| Pull | static/HTTP adapter、快照差异、失败保留旧快照 | 尚不能保证 Pull 全面纠正 Push 后的实际状态 |
| 可观测性 | 日志、指标、审计、告警 | 需要补充未知负载、事件恢复、版本来源等证据 |

本轮前序本地验证：Go 全部测试与 vet 通过、bootstrap 52 条 Python 测试通过、两个示例配置检查通过。审查中的故障复现与静态分析不能替代真实云环境验收；以下问题仍须转化为正式回归测试和修复。

## 3. 按优先级排列的审查问题

### P0：影响摘流安全、容量边界与故障恢复

| 编号 | 位置 | 触发条件及影响 | 修复方向 | 必须通过的验收 |
|---|---|---|---|---|
| R1 | `internal/controller/lifecycle.go`，`FinishDrain` / `workerLoad` | `/get_loads` 失败、负值、缺少 worker 记录时，未知负载可能被当作可结束摘流，提前 Stop | 负载未知时保留 DRAINING；明确 deadline 后的强停策略和审计；Router 丢 membership 不足以证明 worker 无在途任务 | 注入超时/500/缺失记录；deadline 前绝不 Stop；deadline 后记录强停原因 |
| R2 | `lifecycle.go`，`failDrain`；`reconcile.go`；`internal/planner` | 撤销后的 Stop 失败进入 FAILED，后续普通启动/Planner 路径可能重新启用该实例 | 将 PendingRelease/维护意图作为调度硬约束；区分启动失败和摘流失败；恢复原操作 | Stop 连续失败后，不出现 Start/readiness available；恢复后继续归还 |
| R3 | `internal/controller/events.go` | 事件认证确认 partner 身份，但操作实例时未统一确认实例归属；其他 partner 可引用已有 ID | 所有 ADD/UPDATE/REVOKE/SNAPSHOT 处理在事务内检查归属；定义实例 ID 是否全局唯一 | partner B 无法更新或撤销 partner A 的实例，记录拒绝审计 |
| R4 | `events.go`，`store.ClaimEvent`，`reconcile.go` | ClaimEvent 已落库、实例/operation 尚未持久化时崩溃；重投递被当重复，原意图无法恢复 | 持久化可重放事件收件箱；区分接收/处理中/完成/拒绝；通过事务或 durable operation 消除窗口 | 在各持久化边界杀进程，重启后执行一次且仅一次逻辑动作 |

### P1：状态收敛、元数据更新和 Pull 权威性

| 编号 | 位置 | 触发条件及影响 | 修复方向 | 必须通过的验收 |
|---|---|---|---|---|
| R5 | `events.go`，`handleCapacityUpdated` | SERVING 实例 endpoint/lease 改变时先 BeginDrain 并返回，新的元数据尚未可靠保存；service_endpoint-only 更新也未走一致迁移 | 分开保存当前运行身份与待应用身份，摘流旧 worker 后原子切换 | 运行中更新地址/租约，重启后最终注册新地址，旧地址保持不可路由 |
| R6 | `reconcile.go`，LOST 恢复分支 | LOST+SERVING 直接修改为 PREPARING，可能形成非法 PREPARING+SERVING，无法继续恢复 | 明确重新准备前的服务探测/停止与状态转换；不靠单字段赋值恢复 | bootstrap 断连再恢复后，状态合法并重新核验健康/readiness |
| R7 | `reconcile.go`，`Probe` / `settleService` | bootstrap 可达但 SGLang degraded，仍可能维持 SERVING 或重新打开 readiness | 分开容器可达与服务健康；仅服务探测满足条件才注册/恢复流量 | 杀掉 SGLang 子进程而保留 bootstrap，及时关闭调度并记录失败 |
| R8 | `lifecycle.go`，`register`；`internal/routeradapter` | 同 endpoint 从 P 改成 D 时，仅复用 existing worker 可能保留旧角色 | 对比实际 role/model/endpoint；定义 drain 后角色更新协议与 generation | 同地址角色切换后，Router 只出现在正确池，旧角色不接收新请求 |
| R9 | `internal/partner/pull.go`；snapshot confirmed 处理 | Pull 只比较前后两次快照，无法全面纠正 Push 新增/更新产生的额外实际状态；相同快照确认事件被去重 | 每次完整快照与当前有效容量比较；区分内容版本与每次成功观测；权威边界明确 | Push 加入快照外实例，下一次成功 Pull 能纠偏；Pull 失败不撤销 |
| R10 | `internal/partner/http.go`，`list` | HTTP 200 `{}` / `null` 可被解码为空实例集合，引发误撤销 | 强校验 envelope、字段存在性、null/分页/完整性；只有明确完整空集合才视为空容量 | malformed 响应保留旧快照并告警；合法空快照按预期撤销 |
| R11 | `internal/partner/pull.go`，`instanceChanged` | service_endpoint 未参与差异判断，实际服务地址改变未产生 UPDATE | 统一规范化、比较、快照哈希的字段集合 | 只修改 service_endpoint 也触发一次正确 UPDATE |
| R12 | `internal/partner`，`CollectSnapshot` | available 与 active lease 合成时可能忽略仅租约侧存在的运行实例 | 按平台语义合并两个集合并校验冲突；明确 available 是空闲还是全部已分配 | 正在运行、只出现在 active lease 的实例不会被误判撤销 |

### 真实联调新增问题

| 编号 | 实测或代码证据 | 后续处理 |
|---|---|---|
| R13 | Router 的注册接口返回 HTTP 202；`StartService` 随即查询/设置 readiness，worker 尚未可见时进入 FAILED。本轮日志出现该错误，随后出现一次对已启动 Prefill 的多余摘流，最终才收敛为 1P1D | 保持 REGISTERING 并有界等待异步注册完成；Planner 计入已分配但尚在 STARTING/REGISTERING 的角色，避免因暂态容量减少而拆掉唯一 P/D 配对。补充延迟注册测试，再做真实冷启动重跑 |
| R14 | bootstrap 只在启动后 0.25 秒检查子进程，稍后崩溃仍报告 RUNNING；使用 venv Python 但未补 PATH，实际 JIT 报找不到已安装的 ninja | 本轮已完成本地修复与回归测试：status/health/start 刷新进程退出状态；子进程继承受控 venv PATH；控制面遇到 FAILED 快速结束健康等待。远端仍运行基线代码，仅通过启动脚本补 PATH；补丁尚未部署 |

还需确定容量重新供给语义：当前 RELEASED 实例同 ID 再次 ADD 会被忽略。必须区分旧事件重放与同容器新租约，使用实例/租约代际隔离旧任务，不能通过手工清空数据库恢复容量。

## 4. 三节点真实测试方案

### 已确认的环境

| 节点 | 内网地址 | 角色 | 实测资源 |
|---|---|---|---|
| control | 172.19.116.74 | Talea、私有 Router、临时制品分发 | H100 80GB × 1 |
| talea0 | 172.19.116.115 | Prefill、bootstrap | H100 80GB × 4 |
| talea1 | 172.19.111.251 | Decode、bootstrap | H100 80GB × 4 |

三台均为 Ubuntu 22.04 / Python 3.10 / CUDA toolkit 12.8，驱动 580.173.02；未预装推理依赖；容器系统盘约 49 GB。节点间 TCP 可达，但没有暴露 `/dev/infiniband`。这些是本次实测结果，不沿用早前 Python 3.11/PyTorch 预装镜像的假设。

测试模型使用节点已存在的 `/root/public/models/Qwen/Qwen2.5-3B-Instruct`，先每台 1 张 GPU、TP=1、1P1D 验证功能闭环，再扩展并发与多卡。共享目录仅使用现有模型；私有代码与 wheel 放在节点私有 `/opt/tai-talea`。

### 端口与制品

- Talea API：control `127.0.0.1:8787`；SQLite：`/var/lib/tai-talea/tai-talea.db`。
- Router 管理面：control `127.0.0.1:30001`；推理面：control 内网 `:30000`。
- Worker bootstrap：`:9001`，随机共享密钥鉴权；SGLang：`:9002`；Prefill 握手：`:8998`。
- 临时依赖分发：control 内网 `:9080`，只读且带认证；完成分发后关闭。
- 锁定 SGLang fork 提交与 wheel 版本 `0.0.0+g5a26fc1f`，生成依赖清单和每文件 SHA-256。
- 首轮 PyTorch 2.9.1 能完成 GPU 初始化，但无法加载官方 `sglang-kernel 0.4.3`：默认 wheel 依赖 CUDA 13；切换官方 `+cu129` wheel 后，又实测出现 PyTorch C++ ABI 未定义符号。对照官方头文件，相关符号在 PyTorch 2.10 起将行号参数从 `int` 改为 `uint32_t`。
- PyTorch 2.10 进一步暴露 `MessageLogger(SourceLocation, ...)` 符号缺失；对照官方源码，该接口从 2.11 起存在。后续组合因此调整为 PyTorch `2.11.0+cu128` / torchvision `0.26.0+cu128` / `sglang-kernel 0.4.3+cu129`，并使用官方 wheel 的 SHA-256 验证 Torch/Torchvision。fork wheel 的 `cuda-python` 依赖从 13.2.0 改为 12.9.4，单独标记为 `0.0.0+g5a26fc1f.cu12`。这属于可追溯的打包适配，须在最终报告中标明实际验证结果，不能称原始 wheel 未修改即通过。
- fork 不支持早前示例中的 `mooncake_tcp`，当前 Mooncake PD 初始化固定为 RDMA。本轮优先验证原生 NIXL/UCX TCP。若必须修改 fork，须单独记录补丁与新制品版本，不能冒充原始 fork 验收。

### 验收顺序与证据

| 顺序 | 场景 | 通过标准 | 留存证据 |
|---|---|---|---|
| T1 | 环境与制品 | 两个节点 profile 一致、依赖完整、GPU/传输后端可用 | manifest、pip check、版本与探测日志 |
| T2 | Push ADD | 两个实例纳管；重复事件不重复启动；角色最终 1P1D | 请求/响应、实例状态、审计、子进程 PID |
| T3 | 注册与推理 | fork 服务健康、Router 两池正确、返回可解释的非空生成文本 | workers/readiness、聊天响应、P/D 日志 |
| T4 | 摘流与撤销 | readiness 先关闭；在途请求结束后 Stop；不启动新请求 | 带时间戳 SSE/负载/审计日志 |
| T5 | 故障与重启 | 控制面重启不重复启动；Router 重启可重建 membership；bootstrap/SGLang 故障不误判健康 | 重启前后 PID、状态、generation、告警 |
| T6 | 异常回归 | R1–R12 的关键故障测试通过；Pull 部分需协议模拟器配合 | 回归测试与报告，真实测试和模拟测试分开标识 |

实际执行结果见同目录 `three-node-validation-2026-09-18.md`：T1–T3 的本轮功能场景通过；T5 中控制面及 Router 重启通过，其他故障场景未完成；T4/T6 与裸容器全自动铺装仍待验收。

## 5. 分阶段实施与交付

### 阶段 A：真实环境基线与最小推理闭环

先完成本轮三节点部署，确认内部 fork、Router、Talea 的真实组合。交付无密码的部署脚本、版本锁、配置模板、真实请求证据与已知限制。验收门槛为 T1–T3；如果 TCP 后端不成立，应准确记录阻塞点及所需网络/容器能力。

### 阶段 B：P0 正确性修复

依次处理 R1/R2（摘流及撤销意图）、R3（归属边界）、R4（持久化与恢复）。每项先建立能复现故障的测试，再修复；不添加只重复实现细节的测试。交付可审查的小批次变更和故障注入结果。进入下一阶段前，未知负载不得提前停止，已撤销实例不得被 Planner 复活。

按最新自动化要求，阶段 E 中的“首次铺装”必须提前到 A 之后实施，并与 B 的持久化操作/恢复机制衔接。不能等到最后才把人工联调脚本产品化。

### 阶段 C：状态收敛与角色/身份迁移

处理 R5–R8、R13。设计当前运行身份、目标身份和用户/合作商意图的优先级，避免散落的分支各自改状态。增加控制面重启、LOST 恢复、子进程死亡、同地址角色切换、Router 异步注册测试。交付状态转换表和恢复协议，验证 Router membership 与实际进程角色一致。

### 阶段 D：Pull 权威闭环

处理 R9–R12 后才开启真实 Pull。先写清合作商协议：完整性、分页、版本、观察时间、lease 字段、可用集合和在租集合语义。对当前管理状态执行完整快照收敛；区分“查询失败/不完整”与“权威空集合”。交付 adapter 合同测试、Push/Pull 冲突矩阵与可重复的故障演示。

### 阶段 E：可复现铺装

沿用已有 seed/manifest/carrier 设计，先实现最小有用版本：环境探测、兼容类、制品 SHA-256、离线装配、就地创建 venv、失败退出、自检后启动 bootstrap。兼容类包含 OS/CUDA/Python/SGLang commit；manifest 必须再锁定 PyTorch、CUDA Python、sglang-kernel 的版本、构建变体与哈希。自检应实际导入 GPU 内核，不能仅用包版本字符串判定兼容。约束基础镜像已有依赖，避免重复大包。载体按合作商能力选 SSH、私有存储或派生镜像；不把私有 Git 凭据分发给容器。

交付 seed、manifest schema、构建与安装脚本、断点重试、缓存复用和最小故障回滚。当前 SSH 部署脚本是联调用具，不能直接视为正式产品铺装机制。

自动化验收：恢复两个 P/D 节点到未铺装状态（可保留已校验的制品缓存以缩短重跑，但不能保留可直接运行的 venv/bootstrap），只向 Talea 提供容量/连接信息。随后禁止人工远程执行安装和启动，必须由 Talea 自动到达 1P1D SERVING 并完成真实推理。还应在铺装中途重启 Talea，验证操作可以继续、不会重复安装竞争，也不会把安装中的容器误标 LOST。

### 阶段 F：运行与恢复工程化

确定容器实际可用的进程监督机制（本轮没有 systemd，不能直接套 service 文件）；实现重启策略、日志轮转、磁盘预检、受控停止、配置校验。补齐鉴权、端口、secret 文件权限、数据备份与恢复手册。支持人工维护意图和暂停调度，防止人工摘流被周期协调重新打开。

### 阶段 G：规模与 M2/M3

单 Router 正确性与三节点故障矩阵稳定后，再推进多 Router 状态、idle 缓冲、自动补位、灰度升级和回滚。最后接入吞吐/延迟/队列等负载信号进行自适应 P/D；通过滞后、最小持续时间、变更上限避免震荡。性能测试必须区分 TCP 功能验证与 RDMA 生产吞吐目标。

## 6. 排期建议与完成定义

以单人持续开发、平台可访问为前提估计：

| 工作 | 估计投入 | 依赖 |
|---|---|---|
| A：三节点兼容与闭环 | 1–3 人日 | 镜像、网络、fork 依赖可用 |
| B：P0 修复 | 3–5 人日 | 故障复现与持久化语义确定 |
| C：状态与迁移 | 2–4 人日 | B 的意图与恢复模型 |
| D：Pull 权威性 | 2–4 人日 | 合作商 API 契约明确 |
| E/F：铺装与运维 | 3–5 人日 | A 形成可复现制品 |
| G：多 Router/自适应 | 单独评估 | 正确性、压测数据与可用资源 |

约 11–21 人日用于形成可重复验收的单 Router 版本；这些是工作量估计，不包含镜像权限开通、下载故障等外部等待，也不把 M2/M3 全部打包进本轮承诺。

每阶段完成必须具备：实现或明确限制、针对行为的验证、实际日志证据、部署/恢复说明。真实云 API 回收与 static adapter 的管理面回收要分别记录，不能用后者证明前者已接通。

## 7. 无 AI agent 的自动化落地合同

### 7.1 最终运行边界

控制节点部署 Talea、Router、制品目录及进程监督配置。管理员一次性配置平台入口、受支持的运行时版本、模型路径和凭据来源。之后，容量 ADD/UPDATE/REVOKE、Pull 对账与普通节点故障均由 Talea 的持久化控制循环处理。P/D 上的 seed 和 bootstrap 是固定程序，不调用 LLM、不接受临时生成的脚本，也不要求开发者逐台登录。

依赖解析、CUDA ABI 适配、构建和测试属于发布流程；运行期选用已验证且锁定的制品。平台未开放的能力、耗尽的资源或不支持的环境应形成可解释的失败和告警，不把“让 agent 临时排错”作为恢复步骤。

### 7.2 接入现有代码的具体工作

以下均为待实现设计，不能视为当前已有 API。

| 模块 | 具体变更 | 必须保存/暴露的信息 |
|---|---|---|
| `domain` / `config` / PartnerAdapter | 容量身份增加租约代际、铺装连接描述或引用；与 bootstrap/service endpoint 分开。SSH 密钥从控制节点配置读取，事件仅携带 credential_ref | instance、partner、lease、generation、carrier、目标地址、artifact_id/digest；API 和审计不返回密码/私钥 |
| `store` / 事件处理 | ADD 在同一事务内持久化容量意图和准备任务，先返回接收结果；耗时安装由后台处理。区分重复投递与未完成任务 | operation_id、阶段、attempt、next_retry_at、deadline、heartbeat、last_error、目标制品；事件最终结果可查询 |
| 新 `provisioner` / `InstanceManager` | 将首次部署接入 PREPARING；本轮先实现 SSH 载体，通过固定入口下发 seed、manifest、认证材料并读取结构化进度 | 连接成功、探测结果、安装阶段、进度、退出原因；具备取消和超时能力 |
| seed / 制品协议 | 环境与磁盘预检、校验受信 manifest 的签名与每个制品 SHA-256、缓存下载、离线装配、GPU 内核自检、启动 bootstrap | 构建来源、兼容类、包锁、哈希、安装完成标记、自检结果、当前版本；不下发 Git 凭据 |
| Reconciler / Planner | 准备中不按“bootstrap 尚未监听”判 LOST；对任务重新接管；每实例同代际只有一个操作；撤销抢占安装/启动意图 | 远端锁、操作代际、取消标记；不允许旧租约任务在新租约上启动服务 |
| bootstrap / 进程监督 | 以实际子进程和健康探测作为事实；适配当前容器的监督方式，不能假定 systemd 可用；规划 bootstrap 重启后的进程识别或清理再启动策略 | PID 与进程身份、角色/模型、实际版本、退出码；“进程活着”和“可接流量”分别呈现 |

准备任务内部阶段建议为 `PENDING → PROBING → FETCHING → INSTALLING → VERIFYING → BOOTSTRAP_READY`，失败记录在任务上并按策略重试，不增加含糊的容器/服务组合状态。只有健康且版本匹配的 bootstrap 就绪后，才将实例转为 IDLE 并交给 Planner。

venv 在版本化的最终绝对路径就地创建，自检通过后原子切换版本引用，不能把构建机 venv 直接搬运，也不能安装后随意移动目录破坏 shebang。manifest 使用受信发布密钥验证；单独下载到的 SHA-256 不能证明发布来源。

### 7.3 实施顺序与验收门槛

1. 固化本轮可运行的制品、提交 R14 小修复；为 R13 建立延迟注册回归，消除正常冷启动中的多余失败/摘流。
2. 先修 B 阶段的撤销优先级和事件持久化，再实现 E 阶段的最小首次铺装；两者共用持久化任务模型。其余 P0 同批完成。
3. 在两个未铺装 P/D 容器重跑：仅提供容量和连接引用，由 Talea 自动完成铺装、1P1D 分配、注册并成功推理。可保留已校验下载缓存，但不能预留可运行 venv/bootstrap。
4. 在下载、安装和模型启动中分别重启 Talea；恢复后续作，不并发重复安装，不重复起 SGLang。中途 REVOKE 后不得重新 SERVING。
5. 注入凭据无效、制品损坏、磁盘不足、网络暂断、版本不兼容；可重试错误自动恢复，不可重试错误保留原因并告警。
6. 完成 C/D/F：服务故障摘流、角色迁移、Pull 权威收敛、租约重新供给及控制节点重启自启。最后做流式请求摘流、并发、多卡和长时间稳定性验收。

任务结果应由容量 API、operation 状态、日志和指标完整说明；测试脚本只负责输入事件、注入故障和断言结果，不能替 Talea 执行安装、分角色、注册或恢复。
