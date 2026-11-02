# 九章 CCI（AlayaNeW）部署目录

这台平台的部署形态与中国移动 B300 集群（`deploy/kunhaoyuan/`）**对外一致**：
公网只到一个**带 API key 的门面**，推理端口不落在公网上。
不同的地方在于——九章是 CCI 容器，没有 k8s、没有 NodePort、没有 EIP 网关，
所以门面只能跑在容器里（`edge/`）。

> 现场记录与故障定案在 `docs/alayanew-handover-2026-11-02.md`；
> 这里只讲「怎么摆」和「为什么这么摆」。

---

## 1. 平台的三条硬事实（先看，否则会设计错）

| 事实 | 判据 |
| --- | --- |
| **9001 / 9002 是系统端口，永远对公网开放且关不掉** | `POST /cci/instance/{id}/close-port {"port":9002}` → `{"status":100021,"message":"系统端口不可移除"}`。只有自定义端口（8000/8787）能关 |
| **容器里没有 iptables，也没有 TUN 的保证** | `iptables: command not found`；WireGuard 需要 `/dev/net/tun` + `CAP_NET_ADMIN`，**必须先探测**（`edge/cci-edge.sh probe`） |
| **配额是「实例个数」，上限 3** | 下单被拒时报 `40010 …已达上限：3`；`remainingCount` 是平台池快照，不能当配额 |

推论：既然 9001/9002 关不掉，**就不能靠"关端口"来做隔离**，只能让那两个端口上
**没有进程监听**——把 bootstrap 和 sglang 挪到 9101/9102。这是本目录所有配置的前提。

---

## 2. 部署顺序

```
下单 → 铺运行时 → 写配置 → 起控制面 → 起门面 → 验证
 ①      ②          ③          ④          ⑤        ⑥
```

> ②–⑥ 的命令都在**容器内**、且 cwd = `deploy/alayanew/`（路径写成 `ops/...`、`edge/...`）。
> 仓库根目录执行时补上 `deploy/alayanew/` 前缀。

### ① 下单

```
POST /api/osm/v1/subscription/CCI/create
  productCode = skus.cci.gpu.h100.80gb.hbm3.b11.a2.5.11.s20a46b:2      # 8 卡
  cciParams.image = registry.hd-02.alayanew.com:8443/alayanew-public/sglang:20250930
  cciParams.name  = glm53-pd2 / glm53-pd3
  computeResourceType = EXCLUSIVE, priority = 1
  NAS: <nas_storage_id> / glm53-weights → /root/gongji
```

- 新单秒级 `QUEUED`，落卡 1–3 分钟。**ELASTIC 的 GPU 弹性池取不到卡**（CPU 22 秒能落）。
- 实例 id 从 `GET /api/osm/v1/subscription/sku/cci/result/{key}` 取（旧的 `.../CCI/result/` 已停用，返回 `212147`）。
- **订阅 `SUCCESS` ≠ 实例 `RUNNING`**。
- 起来后 `POST /cci/instance/{id}/get-ssh` 拿地址和密码。

### ② 铺运行时（约 2 分钟）

```bash
bash ops/cci-unpack-runtime.sh
```

从 NAS 解 `venv-0.5.22-cu130.tar` + `tai-talea-bits.tar` + `jit-caches.tar`，
先校验 `SHA256SUMS`，最后断言 `sglang 0.5.22 / torch 2.14.1+cu130 / Glm5NextForConditionalGeneration`。
镜像自带的 sglang 是 **0.5.3rc0，不认识 `glm5_next`**，不能直接用。

现役二进制与配置另有一份 `live-2026-11-02.tar.gz`（控制面 `0.9.1-cci` + Router + `/etc/tai-talea/*`），
解包：`tar -xzf live-2026-11-02.tar.gz -C / && chmod +x /root/tai-talea /root/sgl-model-gateway`。

### ③ 写配置

```bash
export TALEA_ADMIN_TOKEN=$(openssl rand -hex 16)
export TALEA_ALAYA_AK=... TALEA_ALAYA_SK=...          # 平台 AK/SK，不入库
export TALEA_ADVERTISE_HOSTS="glm53-pd2=<内网IP>,glm53-pd3=<内网IP>"
bash ops/cci-config.sh
```

`cci-config.sh` 写 `/etc/tai-talea/tai-talea.yaml` 并用 `-check-config` 校验（只校验，不启动）。
**必需手工确认的一处**：`advertise_hosts` 必须是**这次**实例的内网 IP——
实例重建后 IP 会变，写错的表现是「控制面永远连不上 worker」，日志里只有超时。

### ④ 起控制面 + Router

```bash
bash ops/cci-start-plane.sh
```

Router 监听 **`127.0.0.1:8001`**（不是 8000），控制面监听 8787。
8000 留给门面（⑤）。控制面自己按 `planner.gear` 分角色、下发 `POST /bootstrap/start`、
注册 Router。**两台加载约 15–20 分钟**，期间 `/workers` 里 decode `is_healthy=false` 是正常的。

### ⑤ 起门面

```bash
bash edge/cci-edge.sh probe     # 先探测容器能力：/dev/net/tun、CAP_NET_ADMIN、nginx
bash edge/cci-edge.sh up        # 装/起 nginx（wg 仅在本机可用，见 §3.3）
```

见 §3。

### ⑥ 验证

```bash
curl -s localhost:8000/health                                  # 免 key，200
curl -s localhost:8000/v1/models                               # 不带 key → 401
curl -s -H "Authorization: Bearer $TALEA_API_KEY" \
     localhost:8000/v1/models                                  # 200
```

---

## 3. 对外门面（与中国移动同一套形态）

```
客户端 ──TCP 30020──> nginx :8000 ──> Router 127.0.0.1:8001 ──> worker 9102
```

中国移动那套是「WireGuard + nginx」两层；九章只能做到**第一层之后的所有东西**：
单一公网入口、key 校验、推理端口不上公网。**隧道这层做不到**，原因见 §3.3。

| 层 | 作用 | 九章怎么落地 |
| --- | --- | --- |
| 公网入口 | 只暴露一个口 | 平台映射 `8000 → 30020`（自定义端口，可开可关）；**关闭 8787 的映射** |
| 门面 | 校验 API key、限路径 | `edge/nginx-talea.conf.tmpl`：`/health` 免 key，`/v1/*` 要 `Bearer`，`/metrics`、`/server_info` 一律 403 |
| 隧道 | 不把门面直接摊在公网 | ⛔ **九章没有 UDP 映射**（`open-port` 只收 `http/https`，回 `tcp`）⇒ wg0 起得来也没人连得上。见 §3.3 |
| 推理端口 | 不在公网 | `bootstrap_port 9101`、`service_port 9102`——9001/9002 上**没有进程**，映射打空 |

**关掉 8787 的映射**（控制面管理口不该在公网；8000 留着当唯一入口）：

```bash
export TALEA_ALAYA_AK=... TALEA_ALAYA_SK=...
echo '{"port":8787}' | python3 ops/alaya_client.py POST \
  /api/osm/v1/cci/instance/<instance-id>/close-port
# 返回 {"status":200,...} 才算成功；9001/9002 会返回 100021「系统端口不可移除」，这是预期
```

### 3.1 为什么必须挪端口

平台把容器的 9001/9002 强制发布到公网。如果 sglang 还在 9002 上，
任何人都能 `POST /begin_weight_update`——这个端点是 `ADMIN_OPTIONAL`，
没配 key 时对全世界敞开，一调就让 8 个 rank 同时抛
`Blocked unsafe global (builtins.getattr)` 并 `kill_process_tree`。
2026-11-01 的 decode 循环就是这么来的（126 次外部调用 ↔ 122 轮崩溃）。

挪到 9102 之后，9002 上没人监听，NAT 打进来是 connection refused，问题消失。
`--api-key` 也能挡，但它要同时改 bootstrap 的 flag 白名单和 Router 的 worker 注册，
比挪端口麻烦。

### 3.2 门面的三条规则

| 路径 | 行为 |
| --- | --- |
| `/health` | 免 key，200 |
| `/v1/*` | 校验 `Authorization`；`Bearer <key>` 与裸 `<key>` 都接受；**不对是 401** |
| `/metrics`、`/server_info`、其它 | 403（路径没放行，不是 key 的问题） |

401 与 403 含义不同，别混：**401 = 没带/带错 key，403 = 路径没放行**。

### 3.3 隧道为什么搬不过来

中国移动那台是物理机：`wg-quick@wg0` 常驻，防火墙只放一个 **UDP 51820**，
nginx 完全不在公网上。九章有两个叠在一起的阻碍：

1. **平台只发布 TCP。** `open-port` 的请求体是 `{"protocol":"http/https","port":80}`，
   响应固定 `"protocol":"tcp"`（`docs/alayanew-cci.md` §3.3）。没有 UDP 映射，
   就算容器里 `wg0` 起来了，外面的 peer 也**没有地址可以连**。
2. **容器不承诺 TUN。** `/dev/net/tun` 与 `CAP_NET_ADMIN` 都可能没有，
   `edge/cci-edge.sh probe` 会给结论。

所以九章的形态是「**单口 + key**」，不是「隧道 + key」。别在这上面花时间，
除非平台将来支持 UDP 端口映射——那时 `cci-edge.sh up` 和 `wg-add-peer.sh`
（`deploy/kunhaoyuan/ops/`）已经就位，把 peer 加上即可。

推论只有一条，但很重要：**key 是唯一防线**，所以 9101/9102 挪端口不是可选项。
门面退化到 `0.0.0.0:8000` 时，公网上只剩一个需要 key 的 `/v1/*` 和一个 `/health`。

---

## 4. 判据

| 想确认 | 命令 | 正常 |
| --- | --- | --- |
| 门面活着 | `curl -s -o /dev/null -w '%{http_code}' localhost:8000/health` | 200 |
| key 生效 | 不带 key 打 `/v1/models` | **401**（不是 200） |
| 推理端口不在公网 | `curl -m 3 http://<公网IP>:30093/server_info` | 连不上 |
| 9001/9002 空转 | `ss -lntp \| grep -E ':(9001\|9002) '` | 无输出 |
| worker 就绪 | `curl -s localhost:8001/workers` | `prefill_count=1 decode_count=1`，两个 `is_healthy=true` |
| 显存到位 | `nvidia-smi --query-gpu=memory.used --format=csv` | 每卡约 73.5 / 80 GB |

> `/health_generate` 恒 1.0s 是 sglang 代码里的 `sleep(1)`，不是性能问题。
> `bootstrap.log` 会被 200ms 一次的探针刷满，读之前先 `grep -av 'bootstrap/health'`。

---

## 5. 目录

```
deploy/alayanew/
├── README.md                       # 本文件
├── edge/
│   ├── nginx-talea.conf.tmpl       # 门面配置（map 校验 key、SSE 关闭缓冲）
│   └── cci-edge.sh                 # probe / up / down / status，装 nginx、起 wg（本平台到不了）
└── ops/
    ├── alaya_client.py             # 平台 OpenAPI 客户端（HMAC，AK/SK 读环境变量）
    ├── cci-unpack-runtime.sh       # 从 NAS 铺运行时
    ├── cci-config.sh               # 生成并校验控制面配置
    ├── cci-start-plane.sh          # 起 Router(8001) + 控制面
    ├── cci-elastic-probe.py        # 弹性资源探测（下单→轮询→必释放）
    └── cci-probe-sweep.py          # 清遗留实例 + 探测订阅结果接口
```

**凭据一律走环境变量**，仓库是公开的：
`TALEA_ALAYA_AK`、`TALEA_ALAYA_SK`、`TALEA_ADMIN_TOKEN`、`TALEA_API_KEY`、
`TALEA_PUSH_TOKEN`、`TALEA_PUSH_SECRET`。
