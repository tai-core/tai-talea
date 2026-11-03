# 九章智算云 CCI 平台现况 Review 与完善计划书

日期：2026-11-02  
代码基线：`0374135`（`origin/main`）  
范围：九章 CCI 上的 GLM-5.3-Flash 1P1D、控制面、Router、公网门面、NAS 制品、自动铺装与运维体系

> 本计划基于交接记录、当前仓库代码审查和一次真实配置预检。实例释放后，本机没有九章 AK/SK，本次没有重新调用平台在线接口；因此「实例已释放」是最后一次已验证现场，不冒充本次在线复核。所有需要新实例才能确认的结论均标为「待真机」。

---

## 0. 管理层结论

九章平台目前**没有正在提供服务的实例**。旧拓扑曾真实跑通 1P1D，但 decode 因公网 9002 被外部调用打崩；两台实例已于 2026-11-02 15:26 释放。随后仓库完成了 nginx 门面、9101/9102 端口迁移和自动铺装端口参数化，但这些改动**尚未在活实例上验收**。

本次 review 的关键结论不是「还缺生产增强」，而是：

> **新方案的代码方向正确，但当前重建链路存在确定性或高概率硬断点；在修复这些断点并完成 G0/G1/G2 验收前，不应开启自动扩缩容，也不应宣称平台已可重建。**

优先级必须是：

1. 先让重建入口可信；
2. 再分离 NAS 中的秘密与运行时制品；
3. 用真实单节点、双节点重新验收；
4. 补自启、日志、备份、TLS、状态恢复；
5. 最后才恢复自动创建与自动扩缩容。

---

## 1. 证据边界与当前事实

### 1.1 已验证的历史现场

| 事项 | 结论 | 证据 |
| --- | --- | --- |
| 旧 1P1D | 2026-10-26 曾端到端成功，`7*8=56`，0.72 秒 | `docs/alayanew-1p1d-acceptance-2026-10-26.md` |
| 旧拓扑 | Router 8000、控制面 8787、SGLang 9002、bootstrap 9001、PD bootstrap 8998 | 同上 §1 |
| decode 循环根因 | 126 次外部 `begin_weight_update` 与 122 轮崩溃对应；不是模型加载失败 | `docs/alayanew-handover-2026-11-02.md` §7.1 |
| 平台端口限制 | 9001/9002 强制发布且不可移除；容器无 iptables；无 UDP 端口映射 | 交接文档 §6.5/§7.2、`docs/alayanew-cci.md` §3.3 |
| 实例处置 | pd3 15:25、pd2 15:26 release，实例列表 0 条 | 交接文档 §1/§11 |
| NAS 保留 | 权重、运行时 tar、`live-2026-11-02.tar.gz` 留在 NAS | 交接文档 §9 |

### 1.2 当前代码已落地的能力

- `internal/partners/alayanew/adapter.go`：`endpointFor` 支持「平台未发布、但 `advertise_hosts` 已知」的 9101/9102 内网纳管。
- `provision/ssh_install.py`、`install_node.py`：bootstrap/service 端口已参数化，默认仍为 9001/9002。
- `deploy/alayanew/edge/`：nginx `/health` 免 key、`/v1/*` 要 key、其它 403；Router 仅绑 `127.0.0.1:8001`。
- `deploy/alayanew/ops/`：运行时解包、配置、控制面/Router 启动和平台 API 工具已入库。
- 当前 Go `internal/...` 测试、`go vet ./...`、provision 52 项 Python 测试通过；但这些测试没有覆盖下述跨组件重建链路。

### 1.3 本次实际复现

按 `deploy/alayanew/ops/cci-config.sh` 的 heredoc 渲染配置，使用虚构凭据执行真实二进制：

```text
/root/tai-talea -check-config
→ partners[0]: console_token must be at least 16 characters and distinct from all other credentials
```

去掉 `console_token` 行后，同一配置通过：

```text
configuration ... is valid (1 partners, router http://127.0.0.1:8001)
```

因此 P0-1 不是推测，而是**当前配置生成器的确定性缺陷**。

---

## 2. 目标状态

完成本计划后，平台应达到以下可验证状态：

```text
客户端 ──HTTPS──> 平台唯一公网映射 ──> nginx ──> Router 127.0.0.1:8001
                                             ├──> prefill  :9102
                                             └──> decode   :9102

prefill ──PD bootstrap 8998 / Mooncake KV──> decode
控制面 ──> 两台 bootstrap :9101
9001/9002：容器内无监听；公网映射打空
```

并满足：

- 新拓扑双节点真实 1P1D、SSE、公网负例全部通过；
- NAS 上 worker 可读制品不含平台 AK/SK、admin token、bootstrap token；
- 控制面重启、Router 重启、短暂探针失败不会制造状态分裂；
- 日志有轮转，SQLite 有一致性备份和恢复演练；
- 自动创建任务可持久化恢复，自动扩缩容只有在 G3 后才能开启；
- 文档中的每条「已验证」都有对应的真实验收记录。

---

## 3. P0：重建前必须修复的五个问题

### P0-1 配置生成器生成非法配置，且校验失败可能被脚本掩盖

**状态：已修复（2026-11-02，G0 沙箱验收通过）。** `cci-config.sh` 重写为 `set -eu` + 事务写（候选 `tai-talea.next.yaml` → 真实二进制 `-check-config` → 原子 `mv`），删除 `console_token`，`TALEA_BOOTSTRAP_TOKEN` 必填，`image_profile.sglang` 钉住 `0.5.22`。G0 负向注入（token 缺失、非法 host、IP 越界、check 失败模拟）全部非零退出且正式配置未被污染。

原始问题：

**位置：**

- `deploy/alayanew/ops/cci-config.sh`：`admin_token`、`console_token`、`-check-config` 段
- `internal/config/config.go`：`consoleTokens` 校验

**问题：**

- `console_token` 与 `admin_token` 被写成同一个值，违反配置校验。
- 脚本没有 `set -e`；`check_rc` 只打印不退出，末尾 `ls` 可能让脚本最终返回 0。
- 配置直接写正式文件，校验失败后仍留下不可用配置。

**整改：**

1. CCI 不需要 console token 时删除该项；确有需要则生成独立凭据。
2. 配置先写 `tai-talea.next.yaml`，真实 `-check-config` 通过后再原子替换。
3. 校验失败必须非零退出，禁止继续执行 `cci-start-plane.sh`。
4. `TALEA_BOOTSTRAP_TOKEN` 改为 CCI 必填，不再每次随机生成一个只写在控制面的值。
5. 收紧 `image_profile.sglang` 为已验证的 `0.5.22`。

**验收：**G0 中执行真实生成脚本；合法配置通过，token 冲突、非法 host、缺失端口均非零退出，正式配置不被污染。

### P0-2 新配置缺少 PD 的 8998 注册信息

**状态：配置侧已修复（2026-11-02）。** 生成配置现含 `router.prefill_bootstrap_port: 8998` 与 `--disaggregation-bootstrap-port 8998`（extra_sglang_args）。实际注册与 Mooncake KV 传输仍属「待真机」，G2 验收。

原始状态：代码审查已确认；实际故障表现待真机。

**位置：**

- `deploy/alayanew/ops/cci-config.sh` 的 `router:` 与 `extra_sglang_args:`
- `internal/controller/lifecycle.go`：注册时 `BootstrapPort`
- `internal/routeradapter/client.go`：`BootstrapPort == 0` 时不发送字段

**问题：**新 `router:` 只有 name/url/mode/timeout，没有 `prefill_bootstrap_port: 8998`。进程可能健康，但 Router 注册请求没有 prefill 的 KV 握手端口，不能把 `/workers` healthy 当作 PD 可用。

**整改：**

- 显式配置 `router.prefill_bootstrap_port: 8998`。
- 显式配置 SGLang `--disaggregation-bootstrap-port 8998`，让两侧都可审计。
- 检查旧 worker 记录是否更新，避免只改配置却沿用旧注册。

**验收：**Router 注册详情包含正确 8998；真实请求穿过 nginx/Router；日志证明发生 Mooncake/KV 传输，而非只返回进程健康。

### P0-3 NAS prebuilt 运行时与 bootstrap 默认安装语义冲突

**状态：代码侧已修复（2026-11-02）。** 新增 `image_profile.environment_mode: prebuilt|wheelhouse`（含校验测试）；`StartRequest.PrepareEnvironment *bool`（`internal/launcher/client.go` + `start_payload_test.go` 断言 JSON 契约）；controller 在 prebuilt 模式下发 `false`，其余平台默认语义不变。跨组件真实 HTTP 集成测试待 G1。

原始状态：代码路径冲突已确认；NAS 是否碰巧含有可掩盖问题的 requirements 待查。

**位置：**

- `internal/launcher/client.go`：`StartRequest` 没有环境模式
- `bootstrap/tai_talea_bootstrap/server.py`：`prepare_environment` 默认 `True`
- `bootstrap/tai_talea_bootstrap/service.py`：默认调用 `installer.prepare()`
- `bootstrap/tai_talea_bootstrap/environment.py`：强制要求 wheelhouse/requirements.txt

**问题：**NAS 通道的设计是「解包已验证 venv，不再 pip install」，但控制面启动请求未携带 `prepare_environment=false`。即使 NAS 偶然带有 requirements，也会违背 prebuilt 语义并重复安装；没有则在启动模型前失败。

**整改：**

1. 增加明确的运行时模式，例如 `image_profile.environment_mode: prebuilt|wheelhouse`，默认保持 wheelhouse 兼容。
2. `StartRequest` 能表达 prebuilt；CCI 配置明确使用 prebuilt。
3. 不要全局把 bootstrap 默认值改成 false，避免破坏其它平台。
4. 增加跨组件测试：真实 Go StartRequest JSON → bootstrap HTTP handler → prebuilt venv，无 wheelhouse 也能进入 SGLang 启动。
5. 保留 wheelhouse 通道测试，证明原行为不变。

**验收：**G1 在无 wheelhouse requirements 的全新节点上，控制面真实 StartRequest 能启动 9102 SGLang。

### P0-4 手工重建流程没有启动 worker bootstrap，也没有同步 token

**状态：已修复（2026-11-02）。** 新增 `deploy/alayanew/ops/cci-start-worker.sh`（token 必填、preflight、9001/9002 空转检查、启动后带 token 200 / 错 token 401 断言）；README 部署顺序重排为 7 步（解包 → worker bootstrap ×2 → 配置 → 控制面 → 门面 → 验证）；交接文档 §5.2/§6.3 同步更新。真机执行待 G1。

原始状态：代码与文档审查已确认。

**位置：**

- `deploy/alayanew/README.md` 部署顺序
- `deploy/alayanew/ops/cci-unpack-runtime.sh`
- `deploy/alayanew/ops/cci-config.sh`
- `deploy/alayanew/ops/cci-start-plane.sh`

**问题：**现有顺序是解包 → 控制面配置 → 控制面/Router → 门面，没有启动两台 worker 的 9101 bootstrap。配置脚本随机生成 bootstrap token，却没有同步到 worker 的 `/etc/tai-talea/bootstrap-token`。结果可能是无 listener 或全部 401。

**整改：**

1. 新增 `ops/cci-start-worker.sh`：校验 profile、token、二进制，启动 9101 bootstrap，检查鉴权健康和 9001 空转。
2. 控制面与所有 worker 使用同一份受控 token；token 不写入共享 NAS。
3. 重建顺序改为：两台解包 → 两台 bootstrap 就绪 → 控制面配置预检 → 控制面/Router → 门面 → 数据面验收。
4. 长期收敛到修复后的 onboarding，手工脚本只作为灾备路径，避免两套流程漂移。

**验收：**G1/G2 从全新 overlay 执行，无手工补命令；错误 token 返回 401，正确 token 健康，控制面能下发 start。

### P0-5 控制面秘密仍可从 worker 共享 NAS 读取

**状态：脚本侧已修复（2026-11-02，沙箱 31 项正反向测试通过）。** 新增 `ops/cci-pack-runtime.sh`（两个信任域打包：worker 域白名单 `opt/tai-talea` + `profile.json`，成员扫描拒绝 yaml/token/onboarding/db；控制面域 AES-256 加密，缺口令拒绝）与 `ops/cci-restore-plane.sh`（pinned manifest 核 hash → 解密 → 解包，worker 拒绝执行）；`cci-unpack-runtime.sh` 不再自动恢复 live，worker 解完清理旧 tar 带入的控制面配置。**真机执行与第 4 条（凭据轮换）仍待办**：旧 bits tar / live 快照进过的 AK/SK 与全部 token 在下次部署时必须全部换新（部署脚本本就从环境变量取值，重跑 ③④ 即轮换 bootstrap/admin token；平台 AK/SK 需在九章控制台重置）。

原始问题：

**问题：**`tai-talea-bits.tar` / `live-2026-11-02.tar.gz` 可包含控制面 YAML、AK/SK、admin/bootstrap token 和 onboarding 状态。`TALEA_WORKER_ONLY=1` 只删除解包后的副本，worker 仍能读取 NAS 上的原始 tar；`read_only` 不防读取。

**整改：**

1. 将制品拆成「worker 无秘密 runtime」与「控制面私密备份」两个信任域。
2. worker NAS 只保留 venv、bootstrap 源码、JIT cache 及必要无秘密 profile。
3. 控制面配置、token、SQLite、onboarding 状态放到仅控制节点可读的位置，并加密。
4. 轮换已经进入共享归档的 AK/SK、admin token、bootstrap token 及相关 onboarding 凭据。
5. worker 不恢复控制面二进制和 live 配置；控制面恢复 live snapshot 后校验版本/hash。

**验收：**从 worker 实际尝试读取控制面私密备份必须失败；runtime tar 成员扫描无秘密；轮换后旧凭据全部失效。

---

## 4. P1：可用性、可信验收与无人值守前必须完成

| 编号 | 问题与证据 | 整改要点 | 验收 |
| --- | --- | --- | --- |
| P1-1 | `cci-start-plane.sh` 只打印 Router health/listener，未断言新进程和端口 owner；控制面只 `pgrep` | **已修复（2026-11-02）：** 严格检查返回码、listener PID/命令行、Router health、控制面 ready（`/readyz` + pgrep 双断言）；失败非零退出 | 注入端口冲突/假进程/启动失败，脚本均失败且不打印成功 |
| P1-2 | `cci-edge.sh` 直接覆盖正式 nginx 配置；`nginx -t \| tail` 可能掩盖失败 | **已修复（2026-11-02）：** 候选文件 + 真实 `nginx -t` + 原子替换（失败自动回滚备份）；reload 后验证 listener | 错误配置不生效，旧 key 不被误报为已轮换 |
| P1-3 | 控制面、Router、bootstrap、nginx 无守护；容器 STOP/START 后 overlay 还需重铺 | 先确认 CCI entrypoint 能力；否则使用轻量 supervisor。恢复顺序包含制品、token、profile、进程 | kill 各进程能恢复；STOP/START 在测试窗口完成 G4 |
| P1-4 | bootstrap 200ms 轮询刷日志；控制面 `.prev-*`、SGLang/nginx 日志无统一保留策略 | 降低探针频率或分离访问日志；统一大小/时间轮转、压缩、保留上限、磁盘告警 | 构造大量日志后磁盘受控，关键错误仍保留 |
| P1-6 | 公网门面是明文 HTTP + API key，无限流、轮换、撤销 | **部分完成（2026-11-03）：** nginx 门面加 per-IP 限流（20 r/s burst 40、并发 64/IP，server 级使 401 爆破也受限，超限 429）+ 401/403/429 独立告警日志（`edge-authfail.log`）；key 轮换演练真机通过（旧 key 公网 401、新 key 200、reload 不中断）。**TLS 语义已探明：平台 `open-port` 的 `protocol: https` 只是标签，实为 TCP 透传，不做终止**（8443→30119 实测：容器纯 HTTP 时公网 plain 通、TLS 握手失败）。公网 TLS 需域名+真证书在 nginx 终止（裸 IP 只能自签，客户端须关校验）——**待用户决定是否上域名** | 限流/轮换 ✓；公网抓包无明文 key 需 TLS，挂起等决策 |
| P1-5 | live snapshot 不等于控制面备份；SQLite 在 `/var/lib/tai-talea/tai-talea.db`，无一致性备份/恢复演练 | **已完成（2026-11-03）：** 加密私有归档（`cci-pack-runtime.sh` 两信任域，manifest 钉哈希存 NAS 外 `/etc/tai-talea/artifact-manifest.sha256`）+ 隔离恢复演练（解密→解包→`integrity_check ok`→实例/operation/audit 与线上结构一致）+ **每日 `sqlite3 .backup` 定时备份**（04:17 cron，本地 7 天保留，worker 域不可读） | ✓ |
| P1-7 | `/health` 只代表 Router；控制面 `/healthz` 固定 ok；recovery 失败仍 `ready.Store(true)` | **核心已修（2026-11-03）：** recovery 失败不再置 ready——readyz 503（recovering）直到首个无错 reconcile round（once-only hook）；响应体带组件视图 `{recovery, recovery_error, telemetry}`；`/healthz` 保持纯 liveness。低频端到端 synthetic 未做 | 恢复失败 `/readyz` 非 200 ✓；synthetic 待做 |
| P1-8 | STARTING 超过 3 个 reconcile 周期即判无 owner；单次 endpoint 不可达即 LOST | **两项均已修（2026-11-02/03）：** STARTING-stuck 先问 bootstrap（phase=RUNNING 走 resumeStart，§14.3.2）；单次不可达改连败阈值 `lost_failure_threshold`（默认 3 轮，回答恢复即清零；probe 指 bootstrap 口，加载中不受影响） | 重启不假 FAILED ✓；连败阈值测试 ✓ |
| P1-9 | provision job 只在内存 map；QUEUED 永久 outstanding，重启后丢失平台队列跟踪 | 持久化 result key、instance ID、阶段、游标、截止时间；启动恢复轮询；提供继续/取消/核对入口 | 排队超时、控制面重启、重名、长期 CREATING、SSH 失败均不重复创建或泄漏配额 |
| P1-10 | 无内网地址时可能从公网 URL 的外部端口反推容器监听端口 | **已修复（2026-11-02）：** adapter 无 `advertise_hosts` 条目即拒绝纳管；实例模型继续以容器监听端口（9001/9002）为准，公网 URL 仅作参考（回归测试 `TestSnapshotRefusesInstancesWithoutAnInternalAddress`） | 公网 30093→容器 9002 的回归测试中，StartRequest 仍为 9002 |
| P1-11 | 自动扩缩容配置未开，负载读取错误被忽略，webhook 为空 | **已修（2026-11-03）：** 采集器记连败样本（首次/每 10 次/恢复均打日志）+ `LoadInputHealthy`；planner round 在「有实例在服而输入无效」时发 `load_unavailable` 告警（30 个 reconcile 间隔冷却）+ 指标 `planner_load_input_valid`；口径不变：G3 前不开 auto。**webhook 仍为空**（等用户提供 URL） | 负载失败可见 ✓；webhook 挂起 |
| P1-12 | 手工解包缺 SHA256SUMS 仍继续；live snapshot 可能因 bits 中旧二进制存在而不恢复 | **已修复（2026-11-02）：** `cci-unpack-runtime.sh` 缺清单默认拒绝（`TALEA_ALLOW_MISSING_CHECKSUMS=1` 为响亮逃生门）；live 恢复改走 `cci-restore-plane.sh`（显式归档 + hash 核验）；worker 永不恢复控制面 | 归档版本与运行版本一致；错误/缺失清单默认拒绝 |
| P1-13 | NAS 上 SHA256SUMS 与 tar 同属一个信任域 | **已修复（2026-11-02）：** `cci-pack-runtime.sh` 产出 `tai-talea-manifest.sha256`（钉住全部制品 hash，含 SHA256SUMS 自身，指示存 NAS 之外）；`cci-restore-plane.sh` 以它为信任锚，清单缺失/未钉住/篡改均拒绝 | 同时篡改 tar 与 NAS 清单不能通过控制面校验 |

---

## 5. P2：主链路修复后收紧

1. **AlayaNeW 配置校验** —— **已修复（2026-11-02）**：`PartnerAlayaNeWConfig.Validate()` 拒绝 0 值端口与 9001/9002（强制公网对），要求 advertise_hosts 非空且 name/host 合法；10 个拒绝 case + 1 个通过 case（`TestAlayaNeWPortRules`）。
2. **`cci-probe-sweep.py`** —— **已修复（2026-11-02）**：显式 `--result-key` argparse 参数替代 `sys.argv[-1]` 猜测。
3. **WireGuard probe** —— **已修复（2026-11-02）**：`cci-edge.sh` 的 verdict 纳入 CAP_NET_ADMIN 检测结果；本平台无 UDP 映射的口径不变。
4. **配置渲染** —— **已修复（2026-11-02）**：`cci-config.sh` 对 advertise_hosts 逐条 fail-fast（name=ip、重名、IPv4 逐 octet 校验、首尾连字符拒绝），最少 `TALEA_MIN_HOSTS`（默认 2）台；生成后真实 `-check-config` 把关。
5. **文档口径**：历史 10-26 验收保留旧拓扑，不改写；新拓扑只有 G2 通过后才能标「已验证」。

---

## 6. 分阶段实施计划

> 工作量为粗略工程估算，不含平台排队、GPU 空闲等待和跨团队凭据轮换审批。每一阶段必须以上一阶段验收通过为入口。

### 阶段 A：修复重建入口（约 2–3 人日）

**内容：**P0-1、P0-2、P0-3、P0-4、P1-1、P1-2、P1-10、P2 配置校验。  
**产出：**可信配置生成器、worker 启动脚本、prebuilt 启动语义、启动/门面失败闸门、跨组件测试。  
**出口：**G0 全部通过；不申请 GPU 也能证明静态和沙箱链路可信。

**执行结果（2026-11-02）：阶段 A 已完成。** G0 通过，详见 §7 G0 记录。范围外未动：P0-5（阶段 B）；P1-3～P1-9/P1-11～P1-13（阶段 E/F）。

### 阶段 B：重建制品与秘密边界（约 1–2 人日 + 凭据轮换协调）

**内容：**P0-5、P1-12、P1-13。  
**产出：**无秘密 runtime 包、加密控制面备份、独立 hash/签名清单、凭据轮换记录、归档成员清单。  
**出口：**worker 无法读取控制面秘密；控制面可恢复指定版本；旧凭据失效。

**执行结果（2026-11-02）：脚本与信任域边界已完成。** 新增 `cci-pack-runtime.sh` / `cci-restore-plane.sh`，重写 `cci-unpack-runtime.sh`，入仓沙箱测试 `test-stage-b-sandbox.sh` 31 项全绿（打包两域、成员扫描、清单强制/篡改拒绝、pinned manifest 锚点、worker 永不触控制面、错误口令/坏 hash 拒绝）。**待真机/待协调：**① 下次部署在真机上重打包并把 manifest 存到 NAS 之外；② 凭据轮换（bootstrap/admin token 随部署重跑即换新；平台 AK/SK 需控制台重置，旧 live-2026-11-02.tar.gz 与旧 bits tar 中凭据视同已泄漏）。

### 阶段 C：单节点真实铺装 G1（约 0.5–1 人日 + 一台 8×H100）

**内容：**全新 overlay 解包、bootstrap 9101、prebuilt StartRequest、SGLang 9102、9001/9002 空转、版本/hash 验证。  
**出口：**真实解包、真实 bootstrap、真实控制面启动请求、真实 SGLang 监听全部通过；不是 dry-run。

### 阶段 D：双节点新拓扑验收 G2（约 1–2 人日 + 两台 8×H100）

**内容：**完整 1P1D、8998、Mooncake KV、nginx key、SSE、公网负例、Router 补注册、控制面加载中重启。  
**出口：**只有真实 PD 请求与全部负例通过，才把文档改为「9101/9102 新方案已验证」。

### 阶段 E：运维与恢复（约 3–5 人日）

**内容：**P1-3 至 P1-8、日志轮转、分层健康检查、SQLite 备份恢复、TLS/限流/key 轮换。  
**出口：**G4 通过；可以讨论无人值守，但仍不自动扩容。

### 阶段 F：自动创建与扩缩容（约 4–6 人日 + 第三实例）

**内容：**P1-9、P1-11；先验证「已有实例自动铺装」，再持久化创建任务、恢复队列、补告警，最后开 auto。  
**出口：**G3 通过；任何重名、重启、排队超时、失败释放都有可审计行为。

---

## 7. 验收闸门

### G0：本地/一次性 Linux 沙箱

- 真实执行配置生成 + 真实二进制 `-check-config`。
- 断言 9101/9102、Router 8001、8998、两台 advertise_hosts。
- 故障注入证明配置、控制面、Router、nginx 脚本不会假成功。
- prebuilt/wheelhouse 双通道集成测试通过。
- 公网外部端口不再被反推为容器监听端口。

**不通过：不得申请新实例。**

**G0 执行记录（2026-11-02，本地 Windows 沙箱）：**

- 正向：真实 `cci-config.sh` 生成配置 + 真实二进制 `-check-config` 通过（`is valid (1 partners, router http://127.0.0.1:8001)`）；生成结果含 9101/9102、`prefill_bootstrap_port: 8998`、`--disaggregation-bootstrap-port 8998`、`environment_mode: prebuilt`、`sglang: "0.5.22"`、两台 advertise_hosts，且无 `console_token`。
- 负向注入 6 项全部拒绝且正式配置不被污染：bootstrap token 缺失、advertise 单台、重名、IP octet 越界（999）、非 IP、`-check-config` 失败模拟（候选保留、live 未触碰）。
- 管道断言：`cci-start-plane.sh` 断言 Router listener pid == 本脚本启动 pid 且 `/health` 200，控制面 `/readyz` 轮询 + pgrep 双断言；`cci-edge.sh` 候选 → `nginx -t` → 原子替换 → reload 轮询。
- 测试：`go test ./internal/...` 全绿；`go vet` 干净；provision 56 项 Python unittest 全绿（1 项平台相关跳过）；全部 shell `bash -n` 通过。
- **G0 判定：通过。可申请 G1 单节点真机。**

### G1：一台全新 8×H100

- NAS 真实解包与 hash/版本检查。
- 9101 鉴权健康，错误 token 401。
- 无 wheelhouse requirements 也能通过 prebuilt StartRequest 启动。
- SGLang 实际监听 9102；9001/9002 无 listener。
- worker 不能读取控制面私密备份。

**不通过：不得开第二台，不得公网开放门面。**

**G1 执行记录（2026-11-02 晚，两台 8×H800A 实机 glm53-pd2b/pd3b，含 G2 大部分项）：**

- 下单（`cci-order.py`，名字即幂等柄）→ RUNNING 约 1 分钟级；凭据按 §13 全部轮换。
- NAS 真实解包 + SHA256SUMS 强制校验通过；9101 带 token 200 / 错 token 401；
  prebuilt 无 wheelhouse 启动通过；9102 监听、9001/9002 无 listener；
  worker 域无控制面私密（worker-only 解包检出并删除旧 tar 泄漏件，告警生效）。
- 控制面事务化配置 + Router :8001 + 门面（200/401/403）+ 实例纳管全部真实执行；
  现场修 4 个 bug（`2f4bcd1`）。
- **端到端**：门面 `7*8=56`、`9*9=81`×3（0.4–0.6s）；SSE 分块；mooncake KV 传输
  在连接快照中证实（prefill→decode 各 rank P2P+TcpTransport 全 mesh）。
- **公网**：`open-port` 发布门面 8000→30118，无 key 401 / 带 key 200；
  9001/9002 系统端口强制发布但容器内无监听（惰性）。
- **G1 判定：通过**（超出原单机范围，实际在双机上执行）。
- ~~遗留给 G2 收尾~~ **三项已全部完成（2026-11-02 晚）**：
  1. **Router 重启自动补注册 ✓**：新 pid 持 :8001，双 worker 重注册
     （prefill 带 bootstrap_port=8998），无需人工。
  2. **控制面 SERVING 态重启 ✓**：IDLE/SERVING 正确恢复、0 FAILED 标记、
     e2e 132 正常。
  3. **控制面加载中重启不假 FAILED ✓（真机缺陷复现 + 修复 + 回归）**：
     复现见交接文档 §14.3.2——kill decode → STARTING → 加载中重启控制面 →
     旧代码打 FAILED（`no owner for STARTING state`）。修复：reconcile 的
     STARTING-stuck 分支先问 bootstrap，phase=RUNNING 就走 `resumeStart`/
     `finishStart` 等待注册；真机回归 decode 全程 STARTING → 20:14:40 自动
     SERVING → 公网 e2e 200。测试
     `TestRestartedControlPlaneResumesLoadingStartInsteadOfFakingFailed`、
     `TestInterruptedStartWithNothingRunningStillFails`。
     原 P1「decode warmup 放流判据」随 §14.3.1 的归因修正**撤销**：楔死与
     warmup 无关（decode warmup 早于首请求 69 s 完成），系罕见一次性事件，
     abort 自愈 + 调用方重试兜底。

### G2：两台全新 EXCLUSIVE 8×H100

- prefill/decode 各一台，角色、9101/9102、8998 正确。
- Router 注册含 prefill 握手端口。
- 穿过 nginx/Router 的真实请求 200、`finish_reason=stop`，SSE 分块到达。
- 日志证明 Mooncake/KV 传输。
- 公网 9001/9002 连接失败；9101/9102/8001 无公网映射；8787 不可达。
- 不带/错误 key 401；`/metrics`、`/server_info`、`/workers`、权重管理路径 403。
- Router 重启自动补注册；控制面加载中重启不再假 FAILED。

**不通过：保留为测试环境，不宣称新方案可用。**

### G3：第三实例，仅验证自动化

**G3 尝试记录（2026-11-03 凌晨–中午）：未开始，已改期。** 下单 `glm53-pd4b`
（EXCLUSIVE 8×H800A，NAS 随单，create/订阅 SUCCESS）后实例在平台侧 **QUEUED
12.5 小时**（00:23–12:47，`updateTime` 零变化，无排队位次/ETA）——池子无卡，
平台侧不受本系统控制。经用户确认释放回收配额（`POST /cci/instance/{id}/release`
即时生效，实例名不归还）。前置准备已完成：`planner.max_serving: 2` 已在现役
控制面生效；onboarding payload 形状已考证。下次执行从「下单」重新开始，
建议挑池子空闲时段。

- 先设 `planner.max_serving: 2`，避免第三台改变现役 1P1D。
- 已有实例自动铺装纳管。
- 排队超时、控制面重启、重名、SSH 失败、长期 CREATING 都可恢复或明确失败。
- 失败实例按策略释放，不泄漏最多 3 台的配额。
- 负载读取失败有告警。

**不通过：保持 `planner_mode=fixed`、`capacity_actions=false`。**

### G4：故障与恢复

- 分别 kill Router、控制面、bootstrap、SGLang、nginx。
- 测试窗口 STOP/START，验证 overlay 清空后的完整恢复。
- 日志轮转、磁盘告警、webhook 真实到达。
- SQLite 隔离恢复，记录一致，首轮 reconcile 无副作用。
- 再次验证 worker 无法读取控制面秘密。

---

## 8. 风险与回滚

| 风险 | 预防 | 回滚/处置 |
| --- | --- | --- |
| 新配置再次生成非法文件 | `.next.yaml` + 真实预检 + 原子替换 | 保留上一份已验证配置，不启动新进程 |
| 新拓扑 PD 不健康 | G1 后才开第二台；G2 前不开公网 | 保存日志和配置，关闭门面，释放测试实例 |
| 凭据轮换影响现有调用方 | 先列使用方、分批轮换、设置验证窗口 | 在轮换窗口内保留受控回退凭据，完成后必须吊销旧凭据 |
| 第三实例抢占角色或配额 | `max_serving: 2`，固定现役拓扑 | 立即停止 auto，按实例 ID 释放测试节点 |
| 控制面重启误杀正常加载 | 先修 STARTING/LOST，再做重启演练 | 以 bootstrap 真实 phase/PID 为准人工 adopt，禁止盲目 restart |
| NAS 制品被同时篡改 | 控制节点独立 hash/签名 | 停止使用该制品，恢复上一签名版本并轮换相关凭据 |
| TLS/限流变更中断客户端 | 先旁路验证新 listener 和负例，再切映射 | 回退到上一份 nginx 配置并保留旧端口短时间只读验证 |

---

## 9. 交付物与责任边界

| 交付物 | 主要位置 | 责任角色 |
| --- | --- | --- |
| 可信配置生成与 worker 启动脚本 | `deploy/alayanew/ops/` | 平台工程 |
| prebuilt/wheelhouse 启动契约 | `internal/launcher/`、`bootstrap/` | 控制面工程 |
| 双节点验收报告与公网负例 | `docs/alayanew-*-acceptance-*.md` | 平台工程 + 推理工程 |
| 无秘密 runtime 与加密控制面备份 | NAS/控制节点制品目录 | 平台工程 + 安全/运维 |
| 自启、日志轮转、备份恢复手册 | `deploy/alayanew/`、`docs/` | 运维 |
| 自动创建持久化与恢复 | `internal/api/provision.go`、存储层 | 控制面工程 |
| 新方案状态更新 | 交接文档 §1/§6/§7/§10 | 文档 owner；必须有验收证据才能改「已验证」 |

---

## 10. 完成定义（Definition of Done）

### 可称为「新拓扑可用」

- P0 全部关闭；
- G0/G1/G2 有真实记录；
- 公网负例和真实 PD/SSE 全部通过；
- 文档不再把历史旧拓扑结果冒充新拓扑结果。

### 可称为「可无人值守运行」

- 上述条件 + G4；
- 自启、日志、备份恢复、TLS、限流、告警、状态恢复均有验收。

### 可称为「自动扩缩容可用」

- 上述条件 + G3；
- 创建任务持久化恢复、失败释放、配额保护、负载告警全部通过；
- 最后才允许 `mode: auto`、`capacity_actions: true`。

---

## 11. 下一步执行顺序（直接照此推进）

1. 修 `cci-config.sh` 的 token 冲突、校验退出码、8998、显式 bootstrap token。
2. 修 prebuilt `prepare_environment` 契约，并加 Go→bootstrap 集成测试。
3. 补 `cci-start-worker.sh`，重排手工重建顺序。
4. 修控制面/Router/nginx 启动脚本的假成功和原子配置更新。
5. 拆 NAS 秘密与 runtime，钉住制品 hash，安排凭据轮换。
6. 跑 G0；通过后才申请一台实例跑 G1。
7. G1 通过后开第二台跑 G2；G2 通过才更新「新方案已验证」。
8. 再做运维恢复 G4，最后才用第三台做自动扩缩容 G3。

**当前建议：暂停一切「直接开自动扩容」的工作，先完成第 1–6 步。**
