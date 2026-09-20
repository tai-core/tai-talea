# 能力覆盖对照：目标形态 vs 当前实现

> 本文保留 2026-09-17 的历史缺口。SSH 自动上线和网页现已实现，Pull 恢复语义也已补修；当前状态见 [项目说明书](project-guide.md) 和 [最新审查记录](review-2026-09-20.md)。

> 用来回答一个问题：**「把空闲容器自动变成可用的推理节点」这件事，现在做到哪一步了。**
> 不含任何内部评审记录。日期：2026-09-17。

## 1. 目标形态

一台常驻服务器上运行控制面，它持续寻找空闲的容器资源，把 tai-core 的 SGLang 部署进去并
加载模型；合作商侧只暴露两个 **push** 接口——新增空闲资源、回收资源。后期以飞书作为
人看和操作的界面。

## 2. 逐段对照

| 目标环节 | 当前实现 | 结论 |
| --- | --- | --- |
| 跑在一台常驻服务器上 | 单个 Go 静态二进制 + systemd 单元 + SQLite，`deploy/tai-talea.service` | ✅ 已达成 |
| 合作商只给两个 push 接口 | `POST /v1/capacity/events`，事件类型 `CAPACITY_ADDED` / `CAPACITY_REVOKED` | ✅ 已达成 |
| 持续发现空闲容量 | 控制面 10s 常驻循环 + Planner 再平衡轮次，会挑出 `IDLE` 且未派角色的实例 | ✅ 已达成（见 §3.1） |
| **把 SGLang 代码拉进容器** | **无任何传输或安装逻辑** | ❌ **空白** |
| **把容器从裸机变成就绪** | 无。§7.1 第 1/5 步是「验证 endpoint 可达」「验证 bootstrap 版本」——**是验证，不是安装** | ❌ **空白** |
| 部署模型并拉起服务 | 经 `/bootstrap/start` 下发角色与模型路径，启动 SGLang | ⚠️ 依赖容器「已经就绪」这个前提 |
| 摘流与归还 | 只走 Router readiness，全程不调 `DELETE /workers`；在途请求归零或宽限期到期后停服务并释放 | ✅ 已达成 |
| 飞书界面 | 无 | ❌ 空白 |

**一句话**：现在的 tai-talea 是**指挥官**，不是**装修队**。它能把一台已经准备好的容器
从「空闲」完整调度到「正在服务」再干净地归还（这部分很扎实），但把容器从裸机变成
「准备好的」，目前靠人手。

## 3. 三处需要处理的地方

### 3.1 「Pull 轮询」与「只给 Push」是互相冲突的

开发文档 §6.2 明确写「**Pull 对账结果是容量权威来源，Push 只作为实时加速器**」，
分期计划把「Pull 成为权威来源」排在 M2。实现是：Pull 机制完整可用，但默认
`pull.enabled: false`。

如果合作商**只提供 push 接口**，那么 Pull 没有数据源——§6.2 的权威性设计就落空了。
两条出路：

1. **让合作商定期 push 一份全量快照**（`CAPACITY_SNAPSHOT_CONFIRMED`），权威性由快照
   提供，完全不需要 Pull 接口。事件类型与校验都已经实现。
2. 保留 Pull 并在合作商开放查询接口时启用。

⚠️ 但第 1 条**现在只做了一半**：`handleSnapshotConfirmed` 收下快照后只做「审计 + 漂移告警」，
**不会真的回收**快照里缺失的实例。要做到「快照即权威」，这里必须补上实际的对账动作
（否则权威性只是纸面上的）。

### 3.2 容器内准备——最大的缺口

这是从「能驱动一台准备就绪的容器」到「能自己把裸容器变成推理节点」之间的一整段能力。

**为什么原设计没有它**：开发文档 §10 走的是**受控镜像路线**——假设合作商提供
**已经把 bootstrap 和运行环境烤进去**的基线镜像；§9 的 bootstrap 只负责「准备 Python 环境」
（从**容器内已有**的 wheelhouse 装），不负责获取代码；§16 又不复用 dago 的 SSH 通道。
三者叠加的结果是：**没有任何组件负责把东西送进容器**。

**投入产出的三条路**（按性价比排序）：

| 路线 | 做法 | 评价 |
| --- | --- | --- |
| **① `save-image` 固化** | 手工把一台容器装好、验证通过，用九章 `save-image` 存成镜像；以后创建实例选这个私有镜像，天生带 bootstrap + venv | **推荐先做**。一次操作解决所有后续实例，把「每次一小时的手工」变成「一次性投入」。九章的 `save-image` 接口可用，创建实例时确实支持选私有镜像 |
| **② 工具内 SSH 自动 provisioning** | 控制面自己 SSH 进容器：推 bootstrap 包 → 建 venv → 装依赖 → 起 bootstrap | 技术可行。九章支持 SSH 且支持**公钥预注入**（登记一次，以后新实例全部免密），可自动化。**今天实测确认容器内有内网 pip 源（`nexus.cci.alayanew.com`），`pip install sglang` 直连可用——不需要搬运 3~4GB 的 wheelhouse**，这让本路线的成本比原先估计低很多 |
| **③ 让合作商烤进基线镜像** | 对方把 bootstrap 预置进官方镜像 | 最干净，但依赖对方排期，且要同步 `tai-talea-bootstrap/1` 握手串 |

**一个需要澄清的前提**：「拉 **tai-core 自己的** SGLang 代码」具体指什么？

- 若是**标准 sglang 包**（`pip install sglang==0.5.x`）→ 上面三条路都成立，且①最省事；
- 若是**自己的 fork 或定制构建**→ 多一层「私有 wheel 源 / 源码构建」的设计，
  需要单独定（关键约束：容器**访问不了 pypi.org**，只能走内网源或自建源）。

### 3.3 飞书

现在一行都没有（仓库里只有通用的 `alerts.webhook_url` 告警出口）。
参考 dago 的成熟做法——**单向投影 + 机器人通知**，其官方文档明确写着
「不参与服务启动、Router 操作或回滚判断」：

- **多维表格做人的界面**：`Machines` / `Components` / `Alerts` / `Packages` 四张表，
  控制器周期性把状态写进去；只创建或更新记录，**从不删除记录**；
  不创建、删除、重命名表和字段（schema 由人工确认）。
- **机器人发卡片**：告警、rollout、强制停机通知发到群；已恢复的告警在根消息上补一个
  `DONE` 表情。
- 用应用身份的 `tenant_access_token`，**不要**把个人 user token 部署到服务器。

对 tai-talea 来说映射几乎是现成的：`capacity_instances`（容器状态 + 服务状态 + 角色）
对应 Machines，`operations` / `audit_log` 对应过程记录，`alerts` 对应 Alerts。
**告警推群尤其接近就绪**——已经有 `WebhookSink` 和 `alerts.webhook_url`，
飞书机器人 webhook 就是一个 JSON POST。

建议排在真机端到端验证之后，作为独立一步。

## 4. 需要决策的点

1. 「拉自己的 SGLang 代码」是标准包还是自建构建？（决定 3.2 走哪条路）
2. 容器内准备走 ① 固化、② 工具内 SSH、还是 ③ 等合作商？（默认建议先①后②）
3. 合作商是否接受「定期 push 全量快照」？（若是，3.1 的方案 1 成立，Pull 可以继续关着）
4. 飞书排在哪个里程碑？需要几张表、要不要机器人通知？

## 7. 真机验证记录（2026-09-17）

容器：九章智算云 CCI `77c219dc-…`（8×H100 80GB），Ubuntu 22.04.5 / CUDA 12.8 / Python 3.11.13。

### 已打通

| 环节 | 证据 |
| --- | --- |
| 容器内环境就绪 | `sglang 0.5.19` + `torch 2.13.0+cu130`，253 包，venv 8.4G，`pip check` 无冲突 |
| GPU 可用 | `torch.cuda.is_available()=True`，`device_count=8`，`H100 80GB HBM3` |
| 环境契约 | bootstrap `check` → `compatible: true`，mismatches 空，exit 0 |
| bootstrap 启动 | 监听 `0.0.0.0:9001`；日志含 INFO 与「已超出容器回环、共享密钥是唯一屏障」的 WARNING |
| **公网可达** | 平台映射 `9001 → 120.220.102.21:30086`，**从外部机器实测可达** |
| **公网鉴权** | 带令牌 `health`/`status` → **200**；无令牌 → **401**；错误令牌 → **401**；带令牌未知路径 → **404** |
| **原漏洞已关闭** | 公网无令牌 `POST /bootstrap/stop` → **401**（修复前这里会是 200，任何人可停服务） |

### 期间发现并修复的缺陷

**`serve` 完全起不来（提交 `94c9759`）**：`454f1c3`（安全加固那次）重写 `command_serve`
加共享密钥时，把 `load_profile(args.profile)` 写成了 `load_profile(args)`，
整个 argparse `Namespace` 被当路径传给 `os.path.isfile`，`main()` 按内部错误处理 →
真实 `serve` 一律以退出码 70 收场，而 `check` 一直正常。

没有任何测试发现它：**所有测试都直接构造 `BootstrapServer` / `SGLangService`，
从不经过 CLI**；而此前的 live smoke 用的是自写的 `serve.py`，恰好绕开了 CLI。
已补 `ServeCommandTests`（经 CLI 驱动，断言不兼容环境必须返回 65 而非 70），
反向验证：把那一行改回去，恰好这 2 条失败、其余 46 条仍绿。

### `POST /bootstrap/start` 已验证（2026-09-17 追加）

bootstrap 成功拉起 `Qwen/Qwen2.5-3B-Instruct`，SGLang 监听容器 `0.0.0.0:9002`，
**公网 `120.220.102.21:30093/health` 返回 200**，`/v1/models` 正常返回模型信息。
模型加载约 60 秒（load_weight 31s + cuda graph 21s）。

**过程修掉三个缺陷**（提交 `87f5a4c`，加上 `94c9759` 共两轮）：

1. `prepare_environment=False` 时命令用裸 `python3`（系统解释器，没有 sglang），
   而不是 profile 指定的 venv 解释器 → 子进程必死。
2. 「立刻退出」检测是 `time.sleep(0)`，子进程还没死完就检查 → **死服务被报成
   `RUNNING` 且 `exit_code: 0`**。已改为 0.25s 的具名宽限期。
3. `log_file` 被当成 SGLang 参数 `--log-file` 传下去——**SGLang 0.5.x 根本没有这个参数**，
   argparse 直接拒绝，子进程一行输出都没留下就退出了（这也是前两个缺陷显得健康的原因）。
   已改为：`log_file` 是 bootstrap 捕获子进程 stdout/stderr 的目标文件，
   `_spawn` 用重定向替代 DEVNULL——启动失败从此有日志可查。

**部署侧新增一个镜像要求**：`--disaggregation-mode prefill` 会初始化 KV 传输后端，
SGLang **强制要求 `mooncake-transfer-engine`**，没有就 abort（已在容器里装了 0.3.13.post1）。
这是受控镜像要包含的内容，不是代码问题。

### Router 部署与端到端推理已验证（2026-09-17 追加）

**两台九章容器跑通了完整 PD 分离推理链**：

| 项 | 值 |
| --- | --- |
| prefill 容器 | `77c219dc`，内网 `172.19.4.58`，SGLang :9002 + PD bootstrap :8998 |
| decode 容器 | `f317a9ff`，内网 `172.19.111.248`，SGLang :9002 |
| Router | `sglang-router 0.3.2`（**pip 直装，无需编译**），跑在 prefill 容器，:30000 |
| 配置 | `--pd-disaggregation --prefill http://172.19.4.58:9002 8998 --decode http://172.19.111.248:9002` |

**端到端推理成功**：`/v1/chat/completions` 返回「我是Qwen，由阿里云开发的超大规模语言模型…」。

**KV 确实跨了容器**（日志直接证据）：prefill 记录 `Prefill batch, #new-token: 34`，
7 秒后 decode 记录 `Decode batch, #running-req: 1`。且 prefill-only 服务器拒绝普通请求，
所以这个响应**必然**经过了 mooncake KV 跨容器传输——逻辑与日志双重印证。

**三个关键网络/拓扑事实**：

1. **同智算中心的容器内网互通**（`172.19.x.x`），KV 传输走内网即可，
   **不需要为 KV 额外开公网端口**。
2. sglang 的 **PD bootstrap 服务默认端口 8998、只跑在 prefill 上**
   （decode 从路由请求里读 `bootstrap_host:bootstrap_port`），与我们的 9001/9002 无冲突。
3. Router 必须能解析**内网** worker 地址，所以它跑在 DC 内（这次放在 prefill 容器）；
   外部测试走它暴露的公网端口（尚未映射，暂从容器内 curl 验证）。

**部署要点**：`mooncake-transfer-engine` 必须装（PD 模式强制）；传输后端用默认 `mooncake`
（容器无 IB 设备时 mooncake 自动走 TCP），`mooncake_tcp` 已加入 bootstrap 白名单作备选。

### 仍未验证

- **由 tai-talea 控制面驱动**：这次是手工按序调用 bootstrap/Router；
  控制面的动态注册（`POST /workers` + `service_endpoint`）尚未在真机走一遍。
- **摘流不丢在途请求** —— Router 已就位，可以做了。
- **`save-image` 固化** —— 两台容器的环境都已就绪（bootstrap + venv 8.4G + mooncake + router），
  **正是固化成受控镜像的最佳时机**，之后新实例可免装直接用。

## 8. 控制面真机驱动记录（2026-09-17 追加）

用真正的 tai-talea 控制面（Linux 交叉编译二进制，跑在 prefill 容器内）驱动全流程，
Push API 带真实 HMAC 签名推 `CAPACITY_ADDED`。结果与修掉的缺陷：

**已验证 ✓**

| 验收标准 | 结果 |
| --- | --- |
| 新实例能从 ADDED 进入 IDLE | ✓ 两台容器各验证两轮（准备阶段含兼容矩阵校验与 wheelhouse 离线安装） |
| 重复事件不会重复启动或回收 | ✓ 重推同 event_id 返回 `duplicate:true / lifecycle action not repeated`，`capacity_events_total{DUPLICATE}` 计数出现，实例 attempts 不变 |
| 服务能完成启动、健康检查和 Router 注册 | ◐ 启动 ✓、健康 ✓、注册 ✓（worker 落在 :9002、healthy、`bootstrap_port:8998` 成功携带）；**readiness 一步被 Router 缺陷挡住**（见下） |
| 控制面重启后能通过 reconcile 恢复 | ✓ 验证了「收养已在运行服务」路径（重启控制面 → 重新注册 → 不重启服务） |
| 摘流不会使用 DELETE | ◐ 结构上成立（RouterAdapter 接口无 delete 方法，单测断言摘流全程零 DELETE）；**真机验证被同一 Router 缺陷挡住** |

**真机驱动修掉的四个缺陷（各自有提交）**：

1. `Status.started_at` 声明成 `time.Time`，Python 侧发的是 epoch 数字 → 每次
   `/bootstrap/status` 都解不开（`373b66d`）。纯单测抓不到：测试替身序列化的是 Go 形状的时间戳。
2. `waitHealthy` 的窗口继承了 `call_timeout`（20s），而模型加载要 60-90s → 第一次尝试
   判失败、重试撞 422。新增语义正确的 `controller.start_timeout`（默认 10m，`34fe5bc`）。
3. 控制面启动 SGLang 时**没传端口**，bootstrap 默认绑 31000，而注册给 Router 的是
   `service_endpoint`（9002）→ Router 对空地址健康检查失败、几秒内逐出 worker（`df2eca9`）。
   修法：端口从 service_endpoint 解析，绑定与注册永不分歧。
4. 「already RUNNING」被当成失败 → 实例永久无法恢复。改为**收养**：探测 status 确认
   RUNNING 后继续健康等待（`cf299d7`）——这同时就是重启恢复路径。

## 9. Router 缺陷：readiness API 对动态注册的 worker 一律 404（阻塞项）

sglang-router 0.3.2（pip 版）的 `/state/workers/{id}` 与 `/state/workers` 对**动态注册**
（POST /workers）的 worker 一律 404——即使 worker 在 `/workers/{id}` 里存在且 healthy
（已实测，且注册携带的 `bootstrap_port: 8998` 正确出现在 worker 记录里）。

**根因（源码定位）**：readiness 读写的是 `MemoryStateStore`，只有
`core/steps/worker/shared/register.rs`（静态配置/服务发现路径）会 `upsert_worker` 播种；
**动态注册路径 `create_worker.rs` 只写 worker_registry、不播种 state store**。
`PUT /workers/{id}`（update_worker_properties）本可补种，但其异步 upsert 实测未生效。
另外 PD 池生命周期（`PdPoolLifecycleState: Active/Draining/Unhealthy/Removed`）
**没有任何 HTTP 端点暴露**。

**影响**：readiness 门禁（§7.2 SERVING 的最后一步）与 readiness 摘流（§8 的唯一摘流原语）
在动态注册模式下无法工作。§8 禁止 DELETE 作为摘流手段，所以这不是我们能绕过的——
需要 Router 侧修复（create_worker 补 state store 播种，或暴露 PD 池生命周期端点）。

**当前真机状态**：两台容器的 SGLang 正常运行并注册在 Router 中（healthy），
控制面实例停在 IDLE/FAILED（readiness 步骤），Push API/幂等/指标/告警全部工作。
