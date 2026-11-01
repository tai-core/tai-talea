# 把 kunhaoyuan（B300）迁到 tai-talea 控制面：可行性评估

日期：2026-11-01
评估对象：中国移动 B300 集群 `kunhaoyuan`（2 节点 × 8×B300，当前跑 4×TP4 聚合 + 自研 Router）
被评估系统：`tai-talea` v0.8（Go 单体控制面，89 个 Go 文件）

---

## 0. 结论

**技术上可迁，但现在不建议迁。**

真正的结论不是"缺适配器"——`internal/partners/kubernetes/` 已经完整实现，RBAC 和网络也实测通了。
真正的结论是三条：

1. **talea 的价值前提在 kunhaoyuan 不存在。** talea 的核心是"partner 快照 → planner 决策 → 自动扩缩容"。
   kunhaoyuan 是**固定 16 卡、无 Cluster Autoscaler、GPU 弹性拿不到卡**的租户，planner 没有可扩的容量，
   只剩状态机和审计；而这两样用 `glm53ctl.sh` + 一个巡检脚本就能覆盖。
2. **迁过去必须换成 PD 拓扑，而 PD 在 B300 + GLM-5.3-Flash 上是未验证路径。**
   talea 的角色枚举只有 `prefill`/`decode`，bootstrap 无条件注入 `--disaggregation-mode`。
   当前生产是聚合 + MTP（c64 **4782 tok/s**，零失败）；PD 在 B300 上没有做过压测，
   跨节点 RDMA 是否可用还没验，而且 `roles.py:79-80` 的注释自己写着 speculative 在 PD 下起不来。
   **迁 talea = 拿一个已验证的 4782 tok/s 去换一个未验证的拓扑。**
3. **要做完，成本是 3–5 人日（PD 路线）或 2 周起（补 regular 角色路线）**，且收益主要是"运维统一"，不是"指标提升"。

**建议：先做两个 2 小时的实验（PD 可行性 A/B、跨 Pod RDMA），用数据决定；在数据出来之前不动生产。**

---

## 1. 已经证实的事实（本次实测，不是推断）

| 项 | 结论 | 证据 |
| --- | --- | --- |
| k8s partner 是否已实现 | **已完整实现**，1100+ 行含测试 | `internal/partners/kubernetes/{adapter,client,kubeconfig,manifest}.go` |
| 控制面凭据能否建 Pod | **能** | 用 kubeconfig 的 mTLS 三元组直调 apiserver：`POST /api/v1/namespaces/kunhaoyuan/pods` → **201**，`LIST` → 200，`DELETE` → 200 |
| 控制面 Pod 能否连 apiserver | **能** | Pod 内 TCP `192.168.239.2:6443` OPEN、`10.233.0.1:443` OPEN |
| 状态目录能否落盘 | **能** | hostPath `/data02/talea-state` 挂进 Pod 后 `touch` 成功 |
| 会不会误纳管现有 `glm53-*` | **不会** | 控制面 selector = `app.kubernetes.io/managed-by=tai-talea,tai-talea.partner=<id>`；现有 Deployment 只有 `app`/`worker-id` 标签 |
| 端口冲突 | **会冲突** | `talea-router` 是 `hostNetwork: true` + `:8000`；`glm53-router` 也是 `:8000` |
| 跨区镜像仓库 | **拉不到** | 山西 `shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn` 只解析到 IPv6 `2409:8c85:aa0c::acb:d451`，n2 无 IPv6 → TCP 443 FAIL |
| 济南仓库可写 | **可写** | `khy/gongji` 现有 tag：`busybox`、`glm-5.3-flash-base`、`glm-5.3-flash-ailog` |

### 1.1 一个必须记下来的坑：kubeconfig 的 CA 不是 CA

```text
certificate-authority-data 解出来: subject=CN=kube-apiserver issuer=CN=kubernetes
openssl verify -CAfile ca.crt srv.pem → error 20 (unable to get local issuer certificate)
```

也就是说这份 kubeconfig 把**服务端 leaf 证书**当成了信任锚，而不是签发它的 CA。

- **Go 能过**：`x509.Certificate.Verify` 第一步是 `if opts.Roots.contains(c)`（按 DER 字节比较），
  leaf 在 RootCAs 里就直接当链终点，不要求 CA 标志。`kubeconfig.go:240 tlsConfig()` 走的就是这条。
- **curl 能过**：同一逻辑，trust store 里有完全相同的证书即终止验证。
- **Python 3.13 过不了**：`ssl.create_default_context()` 默认开启 `VERIFY_X509_STRICT`，要求信任锚必须是 CA。
  之前 Pod 内探测报的 `CERTIFICATE_VERIFY_FAILED` 是**探测脚本的问题，不是集群的问题**。

结论：迁移不受影响，但任何非 Go/curl 的客户端（Python、Java、运维脚本）对接这个 apiserver 都会踩到；
建议顺手在 kubeconfig 里加 `insecure-skip-tls-verify: true`（`kubeconfig.go:150` 支持），或让平台补真 CA。

---

## 2. 阻断项清单（按严重度）

### B1 · 没有聚合（regular）角色通道 —— 结构性，最贵

三处硬编码把角色锁死成 PD：

```go
// internal/domain/instance.go:65
func (r Role) Valid() bool { return r == RolePrefill || r == RoleDecode }
```
```go
// internal/routeradapter/client.go:190
if !request.WorkerType.Valid() { ... }        // 注册 regular 会被拒
```
```python
# bootstrap/tai_talea_bootstrap/roles.py:11,162
ROLES = ("prefill", "decode")
command += ["--disaggregation-mode", role]     # 无条件注入，无法关闭
```

而 Router 侧其实支持：`smg` 的 `WorkerType` 枚举里有 `Regular`（`sglang-router` 的 `pd_pool.rs:147`）。
**缺的只是 talea 这一侧。**

讽刺的是 `roles.py:79-80` 自己写着：

> Speculative decoding. Only usable in the aggregated layout: the upstream cookbook states
> it does not start under PD disaggregation at this cut.

即：代码知道 MTP 只能在聚合布局用，但 talea 没有聚合布局。

**改造成本**：`RolePrefill|RoleDecode` 在全仓 **172 处、26 个文件（其中 11 个是测试）**出现。
planner 的整个决策模型是"prefill 占多少比例"（`Gears` 1:1 / 2:1 / 1:2，`Decision.TargetPrefill/TargetDecode`，
以及 `prefill↔decode` 的转换动作），加一个"单角色档位"不是加个枚举值，是加一条并行决策路径。
**保守估计 8–10 人日**，且要重跑全部 planner 测试。

### B2 · bootstrap 参数白名单缺 2 个 —— 便宜，但必须改

`ALLOWED_EXTRA_FLAGS`（`roles.py:21-85`）逐条比对当前生产参数：

| 生产参数 | 在名单里 |
| --- | --- |
| `--served-model-name` `--tp-size` `--dsa-prefill-backend` `--dsa-decode-backend` `--kv-cache-dtype` `--moe-runner-backend` `--reasoning-parser` `--tool-call-parser` `--enable-metrics` `--enable-cache-report` `--trust-remote-code` `--max-running-requests` `--speculative-algorithm` `--speculative-num-steps` `--speculative-num-draft-tokens` | ✅ |
| **`--quantization modelopt_fp4`** | ❌ **缺** |
| **`--speculative-adaptive`** | ❌ **缺** |
| `--disaggregation-transfer-backend` `--disaggregation-bootstrap-port` `--disaggregation-ib-device` | ✅（PD 用得上） |

`--quantization` 是 NVFP4 权重的必填项，**PD 和聚合都要**。`--speculative-adaptive` 只有聚合要。
所以：走 PD 路线只需补 1 个；走聚合路线补 2 个。改完要重新烘 bootstrap。

### B3 · worker 镜像契约不满足 —— 需要新镜像

talea 对 worker 镜像的要求（`adapter.go:236`、`deploy/container/entrypoint.sh`）：

| 要求 | 当前 `khy/gongji:glm-5.3-flash-ailog` |
| --- | --- |
| entrypoint `/opt/tai-talea/entrypoint.sh serve` | 无，直接 `bash /opt/talea/start-sglang.sh` |
| python `/opt/tai-talea/venv/bin/python` | 无 venv，系统 `python3` |
| profile `/etc/tai-talea/profile.json` | 无 |
| bootstrap 监听 `:9001`（`/bootstrap/health` 等 4 端点 + `X-Bootstrap-Token`） | 无 |
| sglang 监听 `:9002`（`ServicePort`） | 现在监听 `:30000` |
| 答 `bootstrapVersion = "tai-talea-bootstrap/2"` | 无 |

做法不复杂（bootstrap 是纯 stdlib，`venv/bin/python` 可以软链到 `/usr/bin/python3`，
profile.json 填 os/cuda/python/sglang 版本 + virtualenv），但**必须重打镜像**。

### B4 · 跨区镜像拿不到 —— 必须自建自传

控制面镜像 `shenhaoyuan-...shanxi-1.../library/tai-talea:v0.8` 在山西，n2 **无 IPv6 拉不到**。
济南 `khy/gongji` 可写、n2 已装 `skopeo 1.13.3`，且我们已有 `append-router-layer.py`
（用 registry API 叠层，做过一次：官方 87 层 + 15.3MB 自研 Router 层 = `:glm-5.3-flash-ailog`）。
所以路径是通的，只是"本机无 docker"仍需走 API 叠层或 `skopeo copy`。

### B5 · 控制面清单形态偏弱 —— 便宜，但要改

`deploy/kubernetes/tai-talea-control-plane.yaml`：

- `kind: Pod`（不是 Deployment）→ 节点故障不重调度
- **没有任何探针** → 进程卡死不会被拉起（这正是 nightshift 上 10-15 那次故障的形态）
- `nodeSelector: kubernetes.io/hostname: h100-04` → 迁 kunhaoyuan 必须改
- 挂载 `/data/tai-talea/{config,state}` → 应改 `/data02/talea-*`
- kubeconfig 是 hostPath **file 挂载**（`/data/tai-talea/config/kubeconfig`），
  而 kunhaoyuan 只能用 ConfigMap 塞（我们已验证 ConfigMap 方案可行）

### B6 · `ListAvailableInstances` 语义漂移 —— 当前无害，但是个雷

`partners/kubernetes/adapter.go` 里两个方法**都返回 `listRunning`**。
alayanew 的语义是"空闲容器池"（available）vs"我持有的"（active leases），k8s 这边合并了。
在当前 planner 下无害（它按 active lease 算），但谁按 alayanew 语义当"可分配池"就会重复计数。

### B7 · 纳管边界与端口冲突 —— 迁移时必须二选一

- 标签边界是干净的：现有 `glm53-*` 不会被误纳管。
- 但 `talea-router`（`hostNetwork: true`, `:8000`）与 `glm53-router`（`:8000`）**直接冲突**；
  worker 的 `9001/9002` 与现有 `30000` 也要重新规划。
- 迁移只能是"整套替换"，不能并存。

---

## 3. 两条路线

### 路线 A：PD-only（不改代码角色模型）

**改动面**：B2 补 1 个参数、B3 重打 worker 镜像、B4 自建自传控制面、B5 改清单与配置、B7 换端口。
**不改**：`domain` / `planner` / `routeradapter`。
**成本**：**3–5 人日**（镜像 1.5d、控制面 0.5d、配置与联调 1d、验收 1d）。
**代价**：拓扑从 4×TP4 聚合换成 PD（1P1D×2 或 2P2D），**MTP 用不了**，
吞吐/延迟是未知数，跨节点 KV 传输依赖 RDMA（未验证）。

### 路线 B：补 regular 角色（保留聚合 + MTP）

**改动面**：B1 全部 + B2 补 2 个 + B3/B4/B5/B7。
**成本**：**10–15 人日**（B1 占 8–10 人日，且要新写 planner 的单角色决策路径与测试）。
**收益**：能保住已验证的 4782 tok/s 配置，talea 获得"非 PD 部署"这个能力（对别的环境也有用）。
**风险**：planner 的 `Gears`/`TargetPrefill/TargetDecode`/角色转换三块都要动，
是控制面里最复杂的一块（`planner.go` 800+ 行，测试最多）。

### 推荐

**都不选，先做实验。** 因为路线 A 的成立完全取决于"B300 上 PD 能不能打平 4782 tok/s"，
而这个数据现在没有。用 2 小时换一个决定，比花 5 人日换来一个可能更慢的系统划算。

---

## 4. 建议先做的两个实验（各约 2 小时）

### 实验 1：跨 Pod RDMA 可用性（PD 的前置条件）

在 node7 / node8 各起一个只读挂载的 Pod，检查：

```bash
nvidia-smi topo -m          # 确认节点内 NV18 拓扑
ibv_devinfo                 # 有没有 IB/RoCE 设备
ls /sys/class/infiniband/   # 有几张 HCA、什么速率
```

两 Pod 之间跑 `ib_write_bw`（perftest）。**如果跨节点只有 TCP，PD 的 KV 传输会成为瓶颈，
1M 上下文场景直接不可用** —— 这一条不过，路线 A 直接否掉，评估结束。

### 实验 2：PD 吞吐 A/B

| 组 | 拓扑 | 说明 |
| --- | --- | --- |
| 基线（已有） | 4×TP4 聚合 + MTP + `max_running_requests=64` | **c64 4782 tok/s** |
| A1 | 2×(1P TP4 + 1D TP4)，P/D 分节点 | 16 卡正好铺满 |
| A2 | 1P TP8 + 1D TP8 | 官方高吞吐备选 |

压测条件与基线一致（c64 / c256，多客户端，预热后取稳态）。
判据：**A1/A2 的 output tok/s 至少达到基线的 90%，且 p95 不劣化 20% 以上**，才继续走路线 A。

---

## 5. 如果不迁：现状已经够用的部分

| 需求 | 现状 | 是否够 |
| --- | --- | --- |
| 夜间起、白天停 | `glm53ctl.sh` + cron（20:00 up / 08:00 down），实测 down 21s、up 305s | ✅ |
| 服务自愈 | k8s Deployment + startup/readiness probe；Router `--health-check-endpoint /server_info --health-failure-threshold 5` | ✅ |
| 访问日志 | Router `--enable-ai-access-log` + shell `tee`，已抓 11068 条、0 解析失败 | ✅（缺保留策略） |
| 公网入口 | n2 nginx + API Key + SSE 关缓冲，32 并发全 200 | ✅（缺云安全组放通） |
| 容量调度 | 固定 16 卡，无 autoscaler | 不需要 |
| 审计 / 状态机 | 无 | **唯一真缺口**，但可先用日志补 |

**talea 能带来的净增量只有"审计 + 统一状态机 + 多环境一套控制面"。**
如果这三样不是硬需求，迁过去是用一个已验证的 4782 tok/s 换一个未验证的拓扑。

---

## 6. 什么情况下值得迁

按优先级：

1. **kunhaoyuan 要和 CCI（AlayaNeW）并入同一个容量池**——那时 talea 的 partner 抽象才真正兑现价值
   （一边是 k8s Pod、一边是 CCI 容器，同一套 planner）。
2. **要上 PD 分离做多副本弹性**，且实验 1/2 通过。
3. **合规要求审计留痕**（谁在什么时候把哪个 worker 摘流/释放），且不想自己写。
4. 需要一套 UI/API 统一管理多个集群的容量。

第 1 条是唯一我认为强到足以支撑这次改造的理由。

---

## 7. 附：本次评估新增/修正的认知

1. `partners/kubernetes` **不是待写项**，是已完成项——评估重点因此从"能不能对接"转到"拓扑对不对"。
2. **控制面的就绪判定不看 `/bootstrap/health`**：`lifecycle.go:572 readinessOnce` 走的是
   `router.GetWorker().Healthy` + readiness record。这是好事——PD 下 sglang 的 `/health_generate`
   会因为没有 KV peer 而假 503（AlayaNeW 上踩过），但控制面不受影响。
3. `bootstrap` 的 `/bootstrap/health` 对 `degraded` 也返回 **200**（`server.py:180`），
   而 manifest 的 ReadinessProbe 打的正是它 → **Pod Ready 早于模型就绪**。
   这不会卡住流程（控制器另有一套判据），但别拿 `kubectl get pod` 的 READY 当服务可用。
4. kubeconfig 的 CA 是 leaf 不是 CA（见 §1.1），Go/curl 能过、Python 严格模式不能过。
