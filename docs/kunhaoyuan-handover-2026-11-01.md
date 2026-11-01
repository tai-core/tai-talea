# kunhaoyuan 交接说明（2026-11-01）

这套东西是**已上线在跑的服务**，不是实验环境：4×4 张 B300、一个 GLM-5.3-Flash 的
OpenAI 兼容端点、每天 20:00 自动上电、08:00 自动把卡还给调度器。本文写给**接手运维的人**：
每天该看什么、出问题看哪一行、哪些现象看着吓人但不用管。

工程细节与踩坑记录在 `deploy/kunhaoyuan/README.md`（英文，逐条带实测数字），本文只讲**怎么用**。

---

## 1. 一分钟速览

| 项 | 值 |
| --- | --- |
| 模型 | `glm-5.3-flash`（reasoning 模型，先出 `reasoning_content`） |
| 形态 | aggregated，单角色 `regular`，**4 worker × TP4**，共 16 卡 |
| 集群 | 中国移动 B300，namespace `kunhaoyuan`，两节点各 8 卡 |
| 入口 | WireGuard `10.66.0.1` → nginx `:80`/`:8000` → NodePort `31280` → Router `:8000` |
| 鉴权 | `Authorization: Bearer sk-glm53-…`（**只在 nginx 上校验**） |
| 容量 | 池级上限 **192 并发**（= 4 × `max_running_requests` 48），超出返回 429 |
| 峰值 | 128 并发 **4082 output tok/s**；单流 265 tok/s（TTFT 186 ms） |
| 上电窗口 | 20:00 起，08:00 止；白天池子是关的（卡在调度器里） |
| 跳板 | `36.138.20.89`（内网 `10.5.0.2`），脚本 `/root/taleactl.sh` |

**最重要的一句话**：Router 与 control 常驻，只有 4 个 worker 的副本数在动。
所以"进程都活着但一张卡都没有"是正常状态——Router 能回 `/health` 不等于它能生成，
真正的判据只有两个：真实推理返回 200，且 4 台都在 Router 注册表里 `live`。

---

## 2. 系统长什么样

```
客户端 ──UDP 51820──> wg0 10.66.0.1 ──> nginx :80/:8000
                                            │ least_conn
                                            ▼
                          192.168.239.{2,3,4}:31280  (NodePort, talea-api)
                                            │
                                            ▼
                                  talea-router  8000 服务 / 30001 控制
                                     ▲   │ PUT /state/workers/{id}  ← 摘流唯一原语
                             pull+注册│   ▼
                                talea-control 8787   (常驻)
                                     ▲   │ 按 label 列 Pod / bootstrap 起停
                                     │   ▼
                    talea-w0..w3  (Deployment, 各 4 张 nvidia.com/gpu)
                                     │
                                     └── /data02 上放权重、编译缓存、token 日志
```

三个组件各自的作用，以及**为什么不能一起缩**：

- **talea-router**：对外就是它。OpenAI 兼容接口、池级并发限制、ai 访问日志、
  以及"这台还能不能路由"的状态位。重启它 = 它发给 worker 的内部 id 全换、
  注册表清空，正在服务的 worker 要重新注册（`/bootstrap/start` 会返回
  `already running` 并被 adopt，**不重载模型**，但仍是一次抖动）。
- **talea-control**：把 Pod 变成"实例"、调 worker 的 `/bootstrap/*`、做健康探测
  与摘流。它不持卡，所以常驻。它的 store 是 `emptyDir`：**重启一次实例表就空了**，
  靠下一轮 pull 重新纳管。
- **talea-w0..w3**：真正占卡的东西。一次冷启动约 3 分 40 秒把 40 GB 权重读进 16 张卡。

---

## 3. 每天自动发生的事

跳板机 crontab：

```cron
0 20 * * * /bin/bash /root/taleactl.sh up   >> /var/log/taleactl.log 2>&1
0  8 * * * /bin/bash /root/taleactl.sh down >> /var/log/taleactl.log 2>&1
```

`/root/taleactl.sh` 是**符号链接**指向 `/root/talea-deploy/ops/taleactl.sh`；脚本靠
`readlink -f` 定位同目录的 `worker-registry.py` / `drain-to-router.py`，所以别把脚本
单独拷到别处跑。用 `flock` 串行化：手动执行时正好撞上 cron，后到的那次直接退出 0。

### 20:00 `up` 干的事

1. `patch deploy` 把 4 个 worker 副本数设回 1（`kubectl scale` 这个账号没有权限，403）。
2. 等 **Pod 就绪**——但这一步**不能当判据**：worker 的 readiness 打的是
   `/bootstrap/health`，模型还在加载时它就返回 200。
3. 打一次**真实推理**（`max_tokens=8`），必须 200。
4. 起一个临时 Pod 跑 `worker-registry.py`：对**当前每一个 Pod 的 URL** 要求
   Router 报 `is_healthy` **且**直连 `/health` 应答，缺一台就 `die`。

第 4 步是全部意义所在：一次 200 只证明"有一台能算"（Router 轮询），
4/4 Ready 只证明"bootstrap 起来了"。**在 1/4 容量上跑一整天而面板全绿**，是这套
系统最容易犯也最难发现的错，所以宁可让 `up` 失败。

### 08:00 `down` 干的事

1. **先摘流**：起临时 Pod 调 Router `PUT /state/workers/{id}` 把 4 台标成不可路由，
   然后等 Router 自己记的 per-worker `load`（在飞请求数，流式算到最后一字节）连续两次读到 0。
2. `patch deploy` 缩到 0，等 Pod 真正消失（卡要等容器退出才释放，sglang 需要 60 s 优雅窗口）。
3. `--prune`：删掉注册表里"不在期望内且 `/health` 已不应答"的旧 URL。

摘流期间**新来的请求会拿到 503**，这是预期行为（池子正在消失，给个诚实的 503
好过把连接发给一个正在停的进程）；已经开始的请求不受影响，实测 12 k token 的生成
在 `down` 过程中完整返回 200。

**drain 是 best-effort**：失败只打 WARNING 然后照缩。08:00 的卡必须回来，
一条告警远比"卡回不来"轻。要跳过一次：`TALEA_SKIP_DRAIN=1 taleactl.sh down`。

### `hold`：压测/演示期间别让它动

```bash
/root/taleactl.sh hold 3     # 3 小时内 up 和 down 都不执行
/root/taleactl.sh release
```

`hold` 同时抑制 up 与 down（文件 `/root/talea-hold-until`，epoch 秒，过期自删）。
做压测、迁移、演示之前先 hold，否则 20:00 或 08:00 会把池子从你脚下抽走。

### 实测数字（2026-11-01，0.9.10，这一版交接时的状态）

| 动作 | 耗时 | 证据 |
| --- | --- | --- |
| `down` | **24 s** | 18:50:01 → 18:50:25，`QUIESCE_OK`、Pod 10 s 消失、`pruned=4 kept=0 failed=0` |
| `up`（冷启） | **5 min 50 s** | 18:51:19 → 4/4 `ready + live`，四台一波一起加载 |
| `up`（已在服务） | 29 s | 幂等：重复触发无害 |
| `status` | 15 s | 只读 |

cron 的**环境**也实测过：`env -i PATH=/usr/bin:/bin HOME=/root bash /root/taleactl.sh up`
跑通（所有工具都在 `/usr/bin`，`KUBECONFIG` 缺省即 `/root/.kube/config`）。

> ⚠️ **交接时 `/var/log/taleactl.log` 还不存在** —— 这行 crontab 是 11-01 上午才写进去的，
> 08:00 那次早于它，所以 **11-01 20:00 才是 cron 第一次真实执行**。第一次跑完请确认
> 这个文件出现、且最后一行是 `up: complete, 4/4 pods ready`。

---

## 4. 日常操作

```bash
ssh root@36.138.20.89

/root/taleactl.sh status      # 副本数 / Pod / 占卡数 / 谁在服务 / 端点通不通
/root/taleactl.sh up          # 手动上电（幂等）
/root/taleactl.sh down        # 手动下电（摘流 + 缩容 + prune）
/root/taleactl.sh drain       # 只摘流，池子留着但不接客
/root/taleactl.sh undrain     # 撤销 drain（控制面自己不会把 readiness 写回来）
/root/taleactl.sh prune       # 清死注册（URL 累积会让表越来越难读）
tail -f /var/log/taleactl.log
```

`status` 的五段输出对应五个问题：

- `--- deployments`：副本数与 `readyReplicas`（**不代表性**）。
- `--- worker pods`：Pod 名字与 `kubectl` 的 STATUS 列（`Terminating` 会在这一列出现）。
- `--- cards requested by these pods`：这些 Pod 申请的 GPU 数，正常 16。
  Terminating 的 Pod **照样计数**——容器没退出，卡就还占着。
- `--- workers the router will route to`：`registered=4 router_healthy=4 expected=4` 才算满员。
- `--- endpoint`：真打一次推理。

增加/撤销外部客户端（WireGuard peer）：

```bash
cd /etc/wireguard
bash /root/talea-deploy/ops/wg-add-peer.sh <label> [末位]   # 打印客户端配置（含私钥）
bash /root/talea-deploy/ops/wg-del-peer.sh <label>
```

两个脚本都会重建 `wg0.conf` 并 `wg syncconf`，所以磁盘和运行态不会漂。
配置是**生成式**的：`header.conf`（`[Interface]`）+ `peers/<label>/peer.conf`（`[Peer]`）
→ `wg0.conf`，**不要手改 `wg0.conf`**。`wg-quick@wg0` 已 enable，重启自恢复。

---

## 5. 对外怎么调

隧道通、`ping 10.66.0.1` 有回应之后：

```bash
curl -s http://10.66.0.1/v1/chat/completions \
  -H "Authorization: Bearer $TALEA_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-5.3-flash",
       "messages":[{"role":"user","content":"用一句话解释背压"}],
       "max_tokens":2048}'
```

OpenAI SDK 直接把 `base_url` 指到 `http://10.66.0.1/v1` 即可。

**必须知道的三个坑：**

1. **`max_tokens` 至少 1024，建议 2048。** 这是 reasoning 模型，先吐
   `reasoning_content`，思维链要吃 300~700 tokens。给 512 会得到
   `finish_reason=length` 且 `content` 为空字符串——看着像接口坏了，其实不是。
2. **流式要 `stream_options: {"include_usage": true}` 才有 usage**；否则末帧没有用量。
   首帧实测 0.26 s，109 帧 1.5 s 走完。
3. **429 是正常的背压，不是故障。** 池级上限 192，拒绝会带
   `{"error":{"code":"concurrency_limit"}}` 与 `Retry-After: 1`，客户端按 1 秒退避重试即可。
   429 的 p95 只有 177 ms，不会拖住你。
4. **`x-request-id` 不会原样回显。** 响应头里的 `x-request-id` 永远是 Router 自己生成的值；
   你带来的那个只被记进访问日志的 `caller_request_id`。原因是日志按 `request_id` 落到 ES 文档 ID
   上，而写入是 upsert——同一个 id 来两次，前一次的用量会被静默盖掉。所以**链路追踪请用
   `caller_request_id` 对账，不要拿响应头去比对自己发出去的值**。

`/health` 免鉴权可直接探活（回 200）。`/metrics`、`/server_info` 对外 **403**（只有 nginx
放行 `/v1/*` 和 `/health`）。鉴权只在 nginx 上做——NodePort 本身不校验，但从集群外到不了它。
nginx 用一张 `map` 匹配 `Authorization`，`Bearer <key>` 和裸 `<key>` 都接受；
**key 不对是 401，不是 403**，两者含义不同：401 = 没带/带错 key，403 = 路径没放行。

客户端侧的完整说明（含 WireGuard 客户端导入步骤、实测结果）在
`C:\Users\Tkis1\Documents\talea-api\接入说明.md`，同目录 `test_api.py` 是不依赖第三方库的自检脚本。

---

## 6. 出问题先看这张表

| 症状 | 判据 | 处置 |
| --- | --- | --- |
| 端点 503 `no_available_workers` | Router 日志里 `circuit` 出现 **0** 次？那就是没 worker 可路由，不是熔断 | 跑 `/root/taleactl.sh status` 看 registry；若 08:00 前后，是缩容窗口，等 1 分钟 |
| 端点 429 | body 里 `concurrency_limit` | 正常背压，退避重试 |
| 端点 401 | 没带 key 或带错 | key 在 nginx 的 `map` 里；401 与 403 含义不同，见 §5 |
| 端点 502/504 | n2 上逐个 `curl -m 3 <ip>:31280/health`（3 台 master） | 全不通 ⇒ 集群/NodePort 服务有问题；单个不通 ⇒ nginx `least_conn` 会自动跳过 |
| 隧道不通 | `wg show wg0` 看 `latest handshake`；客户端 `AllowedIPs` 必须**只有** `10.66.0.0/24` | 握手不刷新 ⇒ 查公网 UDP 51820 转发；能握手但打不开页面 ⇒ 查 nginx |
| `up` 中途 `FAILED` | 日志最后一行是哪一类 | `workers did not become Ready` → 看 Pod events；`endpoint never answered` → 看 worker 日志/权重路径；`not every worker registered` → 单台没起来，见下 |
| 4 台里只有 3 台在服务 | `worker-registry.py` 会列出谁 `live`、谁不应答 | 直接 `kubectl -n kunhaoyuan delete pod <那台>`，Deployment 会重建，约 4 分钟 |
| 实例表出现 8 条 / 有 LOST | 每天 08:00 缩容的固定产物 | **等 5 分钟**，`service drain skipped the stop call` → `instance released` 会自愈；不要手工删 |
| 08:00 收到 `instance_lost_rate_high` critical | `detail_rate: 1.000`、`detail_lost: 4` | **预期告警**，缩容必然触发，5 分钟内实例表回到 4 |
| `status` 说 `(not every worker is serving)` 但服务正常 | 满载时探针要排在推理批后面，`/health` 空闲就 1.0 s，registry 的读超时是 5 s | 换个空闲时刻再跑一次；或直接看 `--- endpoint` 那行 |
| Pod `1/1 Running` 但不出话 | readiness 是 `/bootstrap/health`，加载中也 200 | 正常，看 registry 或 worker 日志 |

日志在哪：

```bash
kubectl -n kunhaoyuan logs deploy/talea-control -f     # 摘流、判死、重载的唯一来源
kubectl -n kunhaoyuan logs deploy/talea-router  -f     # 请求级日志（每行一条 JSON）
# token 级 ai-log 落在节点本地盘：/data02/talea-ai/logs/talea-router.log
/root/talea-deploy/ops/export-ailog.sh 2000            # 起只读 Pod 把它捞出来并校验格式
```

token 日志是否符合《第三方网关 Token 日志规范》v1.2、还缺什么、以及后续该由谁采到哪里，
单独写在 `docs/ailog-conformance-2026-11-01.md`；`ops/analyze_ailog.py` 是可复跑的严格校验。

控制面日志里两个关键词的含义：

- `model health probe failed; instance taken out of rotation` —— 探针没答，摘流。
  0.9.10 起**只在实例空闲时计数**，满载下不再误摘。
- `reconcile instance failed` / `instance released` —— 实例被回收。

---

## 7. 已知边界（看到不要慌，也别去"修"）

1. **08:00 会报一次 critical**：`instance_lost_rate_high`。缩容让 4 个实例同时被回收，
   比率天然是 1.000。实例表约 5 分钟内自行收敛回 4 条。**这是当前版本的行为，不是故障。**
2. **`down` 期间新请求 503**：摘流是有意为之；已开始的请求会正常完成。
3. **drain 失败不阻塞缩容**：日志里 `WARNING: drain did not complete cleanly` 说明
   这次没等干净，卡照样还回去了。连续出现才值得查。
4. **control 重启会清空实例表**：store 是 `emptyDir`。下一轮 pull 会重新纳管，
   已经在服务的 worker 走 `/bootstrap/start` 的 "already running" 分支，
   **不会重载模型**，所以不占卡也不用等 4 分钟。
5. **注册表按 URL 累积**：旧 Pod 的 URL 不会被 Router 自动遗忘，`down` 末尾的 `prune`
   负责清。只影响可读性和无用健康检查，不影响路由。
6. **池级 429 不解决偏斜**：如果大量请求共享同一前缀，`cache_aware` 会把它们压到一台，
   这时 192 并发的 TTFT 中位数仍可能到 10 s。要均匀得让客户端打散前缀。
7. **`ready_count` 不会把 Terminating 当就绪**：它读的是 `kubectl get pods` 的 STATUS 列，
   Pod 删除中时这一列是 `Terminating`（虽然 `.status.phase` 仍是 `Running`）。
   这曾经是个怀疑点，11-01 用一个 `terminationGracePeriodSeconds: 600` 的探针 Pod 实测澄清了。
8. **没有 `pods/exec` 和 `pods/portforward`**：读节点本地文件只能起只读 hostPath Pod；
   读 control 的 admin 接口只能临时建 NodePort Service（用完删）。这是平台的 RBAC 边界，
   不是可以绕的配置问题。
9. **cron 到 11-01 20:00 才第一次真跑**：之前只有手动执行记录。
10. **`up` 故意会失败**：4 台里有一台没注册就 `die`，因为"在 1/4 容量上假装成功"
    比失败严重得多。看到 `FAILED` 先看是哪一台。

---

## 8. 交接清单

**需要交接的东西（都不在仓库里）：**

| 东西 | 位置 | 说明 |
| --- | --- | --- |
| 跳板 root | `root@36.138.20.89` | 所有运维入口 |
| kubeconfig | 跳板 `/root/.kube/config` | 只有 master 的 NodePort 可达 |
| admin / bootstrap / push token | 跳板 `/root/.talea-secrets.env` | `apply.sh secrets` 生成 |
| 对外 API key | nginx `/etc/nginx/conf.d/talea.conf`（600） | `sk-glm53-…`，改名会打断已发放调用方 |
| WireGuard 服务端私钥 | `/etc/wireguard/server.key` | `peers/<label>/client.conf` 含各客户端私钥 |
| 镜像仓库 | `kunhaoyuan-62c8ab7d.ecis.jinan-1.cmecloud.cn/khy/gongji` | n2 的 `/etc/hosts` 指 `10.163.118.121` |
| 权重 | 两节点 `/data02/model-weight/GLM-5.3-Flash-NVFP4` | 节点本地盘 |
| 客户端接入说明 + `local-1.conf`（含私钥） | 交接方机器 `C:\Users\Tkis1\Documents\talea-api\` | 面向调用方的文档与首个真实 peer 的私钥，**刻意不入库** |

**代码侧的四个入口：**

- `deploy/kunhaoyuan/` —— 部署清单、`apply.sh`、`taleactl.sh`、`worker-registry.py`、
  `drain-to-router.py`、WireGuard 三个脚本、`bench_serving.py`。
- `deploy/kunhaoyuan/README.md` —— 每个结论的实测证据（英文）。
- `internal/` `cmd/` —— control 与 partner 实现，0.9.10 是当前上线版本。
- `sglang-router`（另一个仓库，Rust）—— 池级限流与 ai 访问日志，镜像里那个
  `sgl-model-gateway` 就是它。

---

## 9. 这次 review 的结论

- **自动启停链路本身没问题**：`down` 24 s、`up` 5 min 50 s（冷启）、幂等重入 29 s，
  判据是"每台都真的在服务"而不是"Pod Ready"。cron 的最小环境也单独验过。
- 修掉了一处**文档与代码不一致**：README 里 `down` 的等待信号还写着读 worker 的
  `sglang:num_running_reqs`，而该指标在这个 sglang build 上根本不存在（会让每次
  drain 白等 5 分钟）。现在文档与 `drain-to-router.py` 一致：读 Router 的 per-worker `load`。
- 澄清了一处**误判**：`ready_count` 不会把 Terminating 的 Pod 算成就绪（实测见 §7.7），
  不需要改代码。
- 未修、只登记：08:00 的 critical 告警（§7.1）、`down` 没有接控制面的
  `POST /v1/capacity/instances/{id}/drain`（多一个失败面可能阻塞缩容，收益不值）。
