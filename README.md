# tai-talea

SGLang 容器化实例资源管控工具 —— 运行在合作商容器平台之上的轻量控制面。

实现依据：`../SGLang容器化实例资源管控工具开发文档.md`。

本模块是**独立模块**：只参考 dago 的经验（operation 状态管理、Router readiness 与健康
检查、sqlite 用法），不依赖 Dago 的 RuntimePackage、元数据模型、SSH/sudo 通道或合作商
资源协议，拥有自己的事件协议、数据模型与 PartnerAdapter 接口（开发文档 §16）。

---

## 0. 名字由来

**tai-talea**（/ˈtɑːleə/）取自中世纪等节奏（isorhythm）术语：一首经文歌里有两个声部，各自按
**不同的周期**重复，谁都不迁就谁——只有被人为保持在同一条时间轴上，整首曲子才不散架。

这正是本项目要做的事：Prefill 与 Decode 是两个节奏不同的声部，容量只在合作商手里起落，
控制面的职责就是让它们始终对齐、并且**摘流时不丢一个在途请求**。

选它之前排除了几个一眼更贴切的名字，因为都已被 Go/容器领域占用：`keel`（keel.sh、
`nauticana/keel`）、`berth`（`rluders/berth` 是同样用"泊位"隐喻的容器管理 TUI）、
`tessera`（`transparency-dev/tessera`）。详见 `.workbuddy/memory/2026-09-17.md`。

---

## 1. 组件地图

| 开发文档中的组件 | 本仓库实现 |
| --- | --- |
| CapacityController | `internal/controller`（事件入口、生命周期编排、Reconcile、Planner 循环） |
| PartnerAdapter | `internal/partner`（`PartnerAdapter` 接口、static 适配器、通用 HTTP 适配器、Pull 对账） |
| InstanceManager | `internal/instancemanager`（§7.1 容器准备与兼容矩阵校验） |
| RuntimeLauncher | `internal/launcher`（容器内 bootstrap 的 `/bootstrap/*` 客户端） |
| RouterAdapter | `internal/routeradapter`（sglang-router 控制面客户端） |
| PDPlanner | `internal/planner`（固定比例 + 手动档位） |
| Reconciler | `internal/controller/reconcile.go`（10s 周期对账与重启恢复） |
| 容器内 bootstrap 进程 | `bootstrap/tai_talea_bootstrap`（纯标准库 Python） |

数据模型与审计：`internal/store`（SQLite，事务化状态写入）。
指标与告警：`internal/obs`（无第三方依赖的 Prometheus 文本格式 + 结构化告警）。
HTTP 面：`internal/api`。

## 2. 快速开始

```bash
# 1. 校验配置（严格模式，未知字段会报错）
make check-config CONFIG=deploy/tai-talea.example.yaml

# 2. 构建
make build          # -> bin/tai-talea

# 3. 运行
./bin/tai-talea --config deploy/tai-talea.example.yaml

# 4. 测试（Go + 容器 bootstrap）
make test
```

容器内（受控镜像路线）：

```bash
# /bootstrap/* 需要共享密钥，容器与控制面两侧必须一致（controller.bootstrap_token）
export TAI_TALEA_BOOTSTRAP_TOKEN=...        # 或在九章创建实例时注入环境变量

python -m tai_talea_bootstrap check --profile deploy/container/profile.json
python -m tai_talea_bootstrap serve --profile /etc/tai-talea/profile.json \
    --host 0.0.0.0 --port 8080
```

**没有密钥就拒绝启动**（退出码 71），而不是退化成无鉴权接口。`--host` 默认是回环地址，
要接受控制面的调用必须显式写 `0.0.0.0`——暴露这个端口是一个需要手写的决定。

## 3. 状态模型（§4）

容器状态与服务状态**分层独立**，组合合法性由 `internal/domain` 集中裁决：

| 容器状态 | 服务状态 | 结论 |
| --- | --- | --- |
| IDLE | NONE | 合法空闲容器 |
| IDLE | FAILED | 合法，等待重试或释放 |
| PREPARING | NONE | 合法 |
| PREPARING | STARTING | **拒绝** |
| IDLE | SERVING（未分配角色） | **拒绝** |
| IDLE | SERVING（已分配角色） | 合法稳态 |
| RELEASING | DRAINING | 合法 |
| RELEASING | SERVING | **拒绝** |
| LOST | 任意 | 合法但**必须告警**，禁止调度 |
| RELEASED | 非 NONE | **拒绝** |

- 非法转换在**事务内**被拒绝（`store.ApplyTransition` 会重新校验），拒绝时写结构化
  日志、计数 `capacity_illegal_transitions_total` 并触发 `illegal_state_transition` 告警，
  **绝不静默修正**。
- 状态变更顺序（§12）：幂等校验 → 状态转换校验 → 更新实例 → 写 operation → 写审计 → 提交。

## 4. API

### 4.1 合作商 Push（§6.1）

```bash
TS=$(date +%s)
BODY='{"event_id":"partner-a:evt-1","partner_id":"partner-a","type":"CAPACITY_ADDED",
       "occurred_at":"2026-09-17T10:00:00Z",
       "instance":{"id":"container-123","endpoint":"http://10.0.0.12:8080",
                   "lease_id":"lease-456","spec":{"gpu":"H100","gpu_count":1,"model_support":["model-a"]}}}'
SIG=$(printf '%s.%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -r | cut -d' ' -f1)

curl -X POST http://127.0.0.1:8787/v1/capacity/events \
  -H "Authorization: Bearer $PUSH_TOKEN" \
  -H "X-Capacity-Timestamp: $TS" \
  -H "X-Capacity-Signature: $SIG" \
  -H 'Content-Type: application/json' \
  -d "$BODY"
```

响应：`{"event_id":"...","accepted":true,"duplicate":false}`（重复投递返回 200 +
`duplicate:true`，**不会**重放生命周期动作）。

Push 侧的安全与幂等由三层组成，缺一不可：

1. **时间戳窗口**（`hmac_tolerance`）：限制一份被截获的请求在多长时间内可用；
2. **HMAC-SHA256**（对 `<timestamp>.<body>` 整体签名）：任何改动都会导致 401；
3. **`event_id` 唯一约束**：重投递命中的是同一个事件行，生命周期动作不会重跑。

因此**完全相同的重投递不是认证失败**：按开发文档 §5「已处理事件返回成功」的要求返回
200 + `duplicate:true`，合作商可以安全重试直到看到这个成功响应。重投递次数记录在
`capacity_push_replays_total` 指标上，便于发现合作商在反复重试。其余失败分别返回
401（签名/凭据）与 400（时间戳超窗、请求体超限）。

### 4.2 运维接口（`Authorization: Bearer <admin_token>`）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/v1/capacity/instances` | 列出实例 |
| GET | `/v1/capacity/instances/{id}` | 单实例 + 合法性判定 |
| GET | `/v1/capacity/instances/{id}/audit` | 审计轨迹 |
| POST | `/v1/capacity/instances/{id}/drain` | 手动摘流（仅 readiness） |
| POST | `/v1/capacity/instances/{id}/release` | 归还容器 |
| GET | `/v1/capacity/events` | 最近事件与处理结果 |
| GET/PUT | `/v1/capacity/planner` | 查看/切换手动档位（1:1、2:1、1:2） |
| POST | `/v1/capacity/reconcile` | 手动触发一轮对账 |

公开端点：`GET /healthz`、`GET /readyz`、`GET /metrics`。

## 5. 关键行为

**摘流不使用 DELETE（§8）**：`RouterAdapter` 接口刻意不提供删除方法；摘流只通过
`PUT /state/workers/{id}` 把 readiness 置为 `unavailable`，`FinishDrain` 在在途请求归零或
超过 deadline 后调用 bootstrap `stop`，之后才通知合作商回收。

**拉取失败不等于容量撤销（§6.2）**：Pull 失败时保留上一次成功快照，产生
`PARTNER_SYNC_FAILED` 告警并累加失败计数；只有全部生成的事件被接受后才提交新快照。

**重启恢复不信任数据库（§13）**：`Recover()` 会逐实例重新探活、核对 Router membership
与 readiness；数据库写着 SERVING 但 Router 不认识该 worker 时，会退回 REGISTERING 并重新
注册，而不会直接标记为 SERVING。

**P/D 比例**：v1 只有固定比例与手动档位，遵守 `max_change_per_round` / `min_hold` /
`cooldown`，优先使用空闲实例，只有空闲容量耗尽才转换在役 worker。负载输入
（吞吐、Prefill/Decode 延迟、Token 速率、队列长度、GPU 利用率、P/D 不平衡度）已作为
`planner.LoadSnapshot` 预留，M3 再接入自适应。

## 6. 指标与告警（§14）

指标名与开发文档 §14 一致，另有少量补充（`capacity_alerts_total`、
`capacity_illegal_transitions_total`、`capacity_open_operations`、`capacity_lost_rate`、
`capacity_serving_instances`）。告警覆盖：Pull 连续失败、LOST 率过高、SGLang 启动失败、
Router 注册失败、readiness 摘流失败、drain 超时、P/D 比例长期偏离、非法状态转换、
租约即将过期、对账失败、回收重试。告警支持日志与 webhook 两种出口，并带冷却去重。

## 7. 相对开发文档的扩展

以最小必要为原则，明确记录如下扩展（其余严格按文档）：

1. `capacity_instances` 增加列：`spec_json`（实例规格持久化）、`role_assigned_at`
   （支撑 `min_hold`）、`pending_release`（区分"摘流后释放"与"摘流后改角色"）、
   `prepare_attempts` / `start_attempts`（重试预算）、`drain_deadline_at`、
   `lease_updated_at`（支撑"过期撤销不得覆盖更新租约"）、`last_error`。
2. 新增 `audit_log` 表：承载"所有容量变化、启动、摘流和回收动作写入审计日志"（§15）。
3. 新增 `POST /v1/capacity/reconcile`、`GET /v1/capacity/planner` 等运维接口，
   供 M1 验收演示与人工介入使用。
4. Pull 对账机制已完整实现并测试，但 M1 默认 `pull.enabled=false`；文档把"Pull 成为权威
   来源"排在 M2，因此这里只保留配置开关。
5. `ROLLBACK`/升级/签名业务包等 M3 内容未实现。
6. **`controller.bootstrap_token`（必填，≥16 字符）**：`/bootstrap/*` 的共享密钥。
   文档没有规定 bootstrap 接口的鉴权，只写了一句"容器内只运行一个受控的 bootstrap 进程"。
   但真实合作商平台（九章智算云）**默认就把容器端口发布到公网**，那样
   `/bootstrap/stop`、`/bootstrap/start`（含任意 `model_path`）会对互联网敞开。
   因此这里补上共享密钥，并让两侧都 fail-closed：

   - 容器侧：密钥为空则拒绝启动（退出码 `71`），默认绑定回环地址；
   - 控制面侧：`controller.bootstrap_token` 少于 16 字符直接拒绝加载配置。

   关于粒度：一个密钥覆盖整个部署，而不是按合作商/实例分发。原因是合作商平台
   只允许在控制台创建实例，密钥必须由人在两侧手工对齐；按实例分发需要程序化创建实例，
   而平台没有开放这个接口。代价是单个密钥泄露的影响面是全部容器，这条权衡记录在此。
7. **`service_endpoint`（可选，事件与静态配置均可携带）**：Router 要访问的 SGLang 地址，
   与 `endpoint`（bootstrap 控制接口）分开。

   文档把实例的"地址"当成一个概念，M1 的实现也一直复用同一个 endpoint——本地测试里
   假容器把 bootstrap 和 SGLang 塞在同一个端口，所以没暴露。接入真实平台后才发现
   九章智算云给的是一个容器端口对应一个公网端口（9001 → `:30086` 给控制接口，
   9002 → `:30093` 给服务），**两个内部端口的外部映射互不相关**，无法从 `endpoint` 推导。

   复用单一 endpoint 会造成两个后果：Router 被注册到一个只会对推理请求回 404 的地址；
   而摘流时按 bootstrap 地址去 `/get_loads` 里找 worker 会永远找不到，控制面把这理解为
   "该 worker 已不在 Router 中，不可能有流量"（`workerLoad` 的兜底分支），
   **于是摘流不会等待在途请求就直接停服务**。

   取值优先级：`service_endpoint` 为空时回退到 `endpoint`，所以单端口部署与本地测试
   行为不变。新增该列时 schema 升到版本 2，并用 `PRAGMA table_info` 做幂等的
   `ALTER TABLE`（`CREATE TABLE IF NOT EXISTS` 不会给已存在的表加列）。

## 8. 里程碑状态

M1 已完成，验收对照见 [`docs/M1.md`](docs/M1.md)。M2/M3 项目（多 Router、Redis 共享状态、
Idle 缓冲、自动补位、PartnerAdapter 插件化、可自定义镜像、灰度回滚、自适应 P/D）均未实现。
