# 九章 CCI（AlayaNeW）交接文档

> 写于 2026-11-02 15:20Z+08。写给接下来接手这台机器的人。
> 这里只写**现场成立的事实**和**踩过的坑**；设计取舍见 `architecture-and-design.md`，
> 容量与配额的取证见 `alayanew-capacity-probe-2026-10-26.md`，
> 自动部署链路的逐段核对见 `alayanew-automation-review-2026-10-27.md`。

---

## 0. 一句话

九章这台是把 GLM-5.3-Flash 用 **1 prefill + 1 decode** 跑起来的验证环境，
`2026-10-26` 验收通过；从 `2026-11-01 00:54` 起 **decode 侧陷入重启循环，服务不可用**，
收尾时按「取证 → 记档 → 释放实例」处理。**它不是生产环境，不需要保活。**

对外形态在收尾时定了案：**门面与中国移动 B300 集群同一套**——公网只到一个带 API key 的
门面，推理端口不上公网（§6.5，脚本在 `deploy/alayanew/`）。**隧道那层没有搬**：
九章只发布 TCP，UDP 51820 无处可通，原因写在 §6.5。

---

## 1. 当前状态（2026-11-02 15:30）

> **收尾结果**：两台实例已于 `2026-11-02 15:26` **release**，实例列表已清空（0 条）。
> 权重与全部运行时制品留在 NAS，下一步要重建按 §6 走，约 25 分钟。
> 下表记录的是**释放前最后一刻**的现场，取证与判据以此为准。

| 项 | 状态 |
| --- | --- |
| 实例 `glm53-pd2` | `RUNNING`，8×H100 80GB，创建于 2026-10-13 10:33 |
| 实例 `glm53-pd3` | `RUNNING`，8×H100 80GB，创建于 2026-10-26 17:06 |
| prefill（pd2） | `SERVING`，sglang 已连续跑 6 天 19 小时；`/server_info` 200 / 9ms，`/health_generate` 200 / 1.0s |
| decode（pd3） | **循环重启中**：加载 → 8 个 rank 同时抛异常 → 进程死 → 重来，每轮约 17.5 分钟 |
| Router（pd2:8000） | 活着，`prefill_count=1 / decode_count=1`，但 decode `is_healthy=false` |
| 真实请求 | `POST /v1/chat/completions` → **503 `No available decode workers`** |
| 控制面 | `tai-talea 0.9.1-cci`，pd2:8787，`planner_mode=fixed`、`capacity_actions=false`、`onboarding_enabled=false` |
| 公网入口（**旧，已废弃**） | `120.220.102.21`：30020→Router 8000（**无鉴权**）、30021→控制面 8787、30086/30093→pd2 的 9001/9002、30002/30003→pd3 的 9001/9002 |

> 上表最后一行就是事故面：Router 和两个 sglang 端口全在公网且无鉴权。
> **重建时不要再照这个摆**——对外改成与中国移动同一套门面（§6.5），
> 部署脚本在 `deploy/alayanew/`。

**收尾时的判断**：这台机器**本身是好的**——pd2 的 prefill 连续跑了 6 天 19 小时，
pd3 每一轮都能把 305 GB 权重加载完、把 PD warmup 走完。坏的是**暴露面**：
9002 在公网、`/begin_weight_update` 无鉴权，端口一开就被外面调崩（§7.1）。

`2026-10-26` 那次 1P1D 验收是真的（`7*8=56`、0.72s），`11-01 00:54` 之前也一直能服务。
所以不是「九章跑不了这个模型」，是「九章默认把推理端口摊在公网上」这件事后来被人踩到了。

---

## 2. 平台与账号

| 项 | 值 |
| --- | --- |
| 平台 | 九章智算云（AlayaNeW）CCI，北京二区 `aidcId=2` |
| API | `https://api.alayanew.com`，HMAC-SHA256 签名（`{ts_ms}\|{METHOD}\|{uri}`） |
| 凭据 | AK/SK 在控制面配置 `/etc/tai-talea/tai-talea.yaml` 的 `partners[0].alayanew`；**不入库** |
| 配额 | **按「实例个数」计，上限 3**。不是 16 卡（取证见 `alayanew-capacity-probe-2026-10-26.md`） |
| 库存接口 | `GET /api/osm/v1/product/list-for-cci?aidcId=2` 的 `remainingCount` 是**平台池余量快照**，不是配额 |
| 镜像 | `registry.hd-02.alayanew.com:8443/alayanew-public/sglang:20250930`（Ubuntu 22.04 / CUDA 12.9 / python 3.12 / sglang **0.5.3rc0**） |
| NAS | `nas_storage_id=c1efadd1-…`，挂到 `/root/gongji`，含权重与运行时制品 |

> 镜像不能直接用：0.5.3rc0 没有 `Glm5NextForConditionalGeneration`，加载会在第一步失败。
> 真正跑起来的是从 NAS 铺进去的 `sglang 0.5.22 + torch 2.14.1+cu130`（见 §6.2）。

---

## 3. 拓扑

**重建后应当长这样**（对外形态与中国移动 B300 集群一致，**隧道那层除外**）：

```
客户端 ──TCP 30020──> nginx :8000 ──> Router 127.0.0.1:8001 ────────────────────┐
        （唯一公网入口，校验 API key；无隧道，理由见 §6.5）                      │
                                                                                 ▼
                    ┌────────────────────────────────────────────────────────────┘
                    ▼                                        ▼
        pd2:9102  prefill (sglang 0.5.22)         pd3:9102  decode (sglang 0.5.22)
                    │           ▲                            │           ▲
                    │           └── mooncake KV ─────────────┘           │
                    ▼                                                    ▼
        pd2:9101  bootstrap（控制面下发 start/stop）          pd3:9101  bootstrap

        pd2:8787  tai-talea 控制面（只监听容器内，不发布）
```

平台映射只剩一条：`8000 → 30020`，落点是 nginx。
**`9001`/`9002` 上没有任何进程**——平台仍然把它们发布到公网，但 NAT 打进来是
connection refused。这是 §7.2 唯一可行的解法。

历史现场（2026-11-02 释放前）是另一种摆法：Router 直接占 8000、sglang 占 9002、
控制面占 8787，三个口全在公网。**不要再照那个摆**，理由见 §7.1。

- 控制面和服务**不在数据面上**：控制面挂了不影响已注册的 worker 出 token。
- Router 重启不会留空表：`reconcileServiceMembership` 每轮比对，实测 25 秒内自动回位。
- 两台容器的内网地址分别是 `172.19.166.190`（pd2）、`172.19.232.225`（pd3），
  写在控制面配置的 `advertise_hosts` 里。**重建实例后 IP 会变，这一条必须改。**

---

## 4. 机器上跑着什么

| 路径 | 内容 |
| --- | --- |
| `/root/tai-talea` | 控制面二进制（`0.9.1-cci`，commit `0b7b5e4`） |
| `/root/sgl-model-gateway` | Router 二进制 |
| `/etc/tai-talea/tai-talea.yaml` | 控制面配置（**含平台 AK/SK**，worker 从不读它） |
| `/etc/tai-talea/profile.json`、`bootstrap-token` | worker 自检口径与 bootstrap 鉴权 |
| `/opt/tai-talea/venv` | 真运行时：sglang 0.5.22 + torch 2.14.1+cu130（约 7 GB） |
| `/opt/tai-talea/bootstrap-src` | worker 侧 bootstrap 包（用系统 python3 跑，不用 venv） |
| `/opt/tai-talea/logs/` | `control-plane.log`、`router.log`、`bootstrap.log`、`sglang-{prefill,decode}.log` |
| `/var/lib/tai-talea/tai-talea.db` | 控制面 SQLite（实例/operation/audit） |
| `/root/gongji/GLM-5.3-Flash` | 权重：62 shard、306 GB、76108 张量、FP8 block-wise |
| `/root/gongji/tai-talea-runtime/` | 烤好的运行时：`venv-0.5.22-cu130.tar`(11.2G) + `tai-talea-bits.tar`(27.7M) + `jit-caches.tar`(122M) + `SHA256SUMS` |

**overlay 会丢，NAS 不会**：容器 `STOP → START` 之后 `/etc/tai-talea`、`/opt/tai-talea`、
`/root/tai-talea` 全部消失，只剩 `/root/gongji`。这是 N3 缺陷，不是故障（见 §8）。

---

## 5. 日常操作

### 5.1 看状态

```bash
# Router 名册
curl -s http://127.0.0.1:8000/workers | python3 -m json.tool

# 控制面视角
T=<admin_token>
curl -s -H "Authorization: Bearer $T" http://127.0.0.1:8787/v1/capacity/instances
curl -s -H "Authorization: Bearer $T" http://127.0.0.1:8787/v1/capacity/console
```

判据：

| 想确认 | 看什么 | 正常值 |
| --- | --- | --- |
| 进程活着 | `ps -eo pid,etime,args \| grep -E 'sglang\|tai-talea\|gateway'` | 三个都在 |
| 显存占到位 | `nvidia-smi --query-gpu=memory.used --format=csv` | **每卡约 73.5 / 80 GB**；只占 40 GB 说明还在加载 |
| prefill 可用 | `curl -o /dev/null -w '%{http_code}' http://<ip>:9002/server_info` | 200，约 9 ms |
| 模型能生成 | `curl -o /dev/null -w '%{http_code}' http://<ip>:9002/health_generate` | 200，**恒 1.0s**（代码里有 `sleep(1)`，不是慢） |
| Router 认它 | `/workers` 里 `is_healthy` | true |
| 端到端 | `POST /v1/chat/completions` | 200 且 `finish_reason=stop` |

### 5.2 起 / 停

```bash
# cwd = deploy/alayanew/（脚本已入库，2026-11-02 收尾时收的）；日常就这四条
bash ops/cci-unpack-runtime.sh   # 铺运行时（容器重建后必跑，约 2 分钟）
bash ops/cci-config.sh           # 写 /etc/tai-talea/tai-talea.yaml 并校验（凭据走环境变量）
bash ops/cci-start-plane.sh      # 起 Router(127.0.0.1:8001) + 控制面，控制面自己拉 sglang
bash edge/cci-edge.sh up         # 起对外门面（nginx；wg 在本平台是空转，见 §6.5）
```

停：`pkill -x tai-talea` / `kill <router pid>`。控制面不托管 Router，两者分别起停。
**注意 `pkill -x sgl-model-gateway` 打不中**——`comm` 只有 15 字符，实际是 `sgl-model-gatew`，
脚本里用的是 `ps -eo pid,cmd | grep '[s]gl-model-gateway launch'`。

### 5.3 排障入口

```bash
# 控制面：谁被判死、为什么
grep -a 'reconcile instance failed\|alert' /opt/tai-talea/logs/control-plane.log | tail -20

# worker：bootstrap 侧看到的(start/stop/health)
grep -av 'bootstrap/health' /opt/tai-talea/logs/bootstrap.log | tail -40

# sglang 自己的死法
grep -a 'Scheduler hit an exception\|Traceback\|fired up and ready' /opt/tai-talea/logs/sglang-decode.log | tail
```

> `bootstrap.log` 会被控制面的健康探针刷屏（每 200 ms 一次），一天 100 MB+。
> **先 `grep -av 'bootstrap/health'` 再读**，否则看到的全是 200。

---

## 6. 重建：从零到出 token

### 6.1 下单

```
POST /api/osm/v1/subscription/CCI/create
  productCode = skus.cci.gpu.h100.80gb.hbm3.b11.a2.5.11.s20a46b:2   # 8 卡
  cciParams.image      = registry.hd-02.alayanew.com:8443/alayanew-public/sglang:20250930
  cciParams.name       = glm53-pd2 / glm53-pd3
  computeResourceType  = EXCLUSIVE, priority = 1
  NAS: c1efadd1-… / glm53-weights → /root/gongji
```

- 新单秒级 `QUEUED`，落卡通常 1–3 分钟；**弹性（ELASTIC）GPU 取不到卡**，别试。
- 实例 id 从 `GET /api/osm/v1/subscription/sku/cci/result/{key}` 取（旧路径 `.../CCI/result/`
  已停用，返回 `212147`）。**订阅 `SUCCESS` ≠ 实例 `RUNNING`**。
- 起来后 `POST /cci/instance/{id}/get-ssh` 拿 `ssh root@120.220.102.21 -p <port>` 和密码。

### 6.2 铺运行时（约 2 分钟）

```bash
bash deploy/alayanew/ops/cci-unpack-runtime.sh
# 先 sha256 -c，再解 venv/bits/jit-caches，最后断言：
#   sglang 0.5.22 / torch 2.14.1+cu130 / Glm5NextForConditionalGeneration OK
```

⛔ **不要**用镜像自带的 sglang，也不要 `pip install` 现装（25 分钟，且 pypi 拉 torch 会踩版本）。
⛔ `tar --exclude` 在 create 模式是**位置相关**的，写在成员路径后面会静默失效，
曾把 venv 又打一遍（11 GB 而不是 200 MB）。

### 6.3 起控制面

```bash
bash deploy/alayanew/ops/cci-config.sh        # 校验配置（会打印 check_rc）
bash deploy/alayanew/ops/cci-start-plane.sh   # Router + 控制面
```

⚠️ 起控制面前**先确认没有手工起的 sglang 占着显存**（8×73.5 GB），否则控制面再起会 OOM。
⚠️ 改过 `advertise_hosts` 之后必须重启控制面；它只在启动时读一次。

### 6.4 控制面会自己做的事

1. 每 10s reconcile 一次，两台实例分 prefill / decode（`planner.gear: "1:1"`）
2. 通过 `POST http://<ip>:9101/bootstrap/start` 下发启动（异步 202）
3. 等 `/bootstrap/health` 变 `ok` 且 phase `RUNNING`，再注册到 Router
4. 注册成功后置 `SERVING`

**加载时间**：`2026-10-26` 实测 19 分钟（305.8 GiB / 76108 张量 / 317 MB/s，
上限是 CephFS 冷读 373 MB/s，加 concurrency 不会更快）；`2026-11-02` 缓存热后约 14–15 分钟。

### 6.5 对外入口（与中国移动同一套）

```bash
export TALEA_API_KEY=$(openssl rand -hex 16)
bash deploy/alayanew/edge/cci-edge.sh probe    # 这个容器能起 wg 吗（起了也到不了，见下）
bash deploy/alayanew/edge/cci-edge.sh up       # 渲染 nginx、起门面
bash deploy/alayanew/edge/cci-edge.sh status   # 三条判据一次看完
```

形态：

| 层 | 九章的落点 |
| --- | --- |
| 平台映射 | **只留 `8000 → 30020`**；`close-port` 掉 8787（自定义端口能关） |
| 门面 | nginx：`/health` 免 key、`/v1/*` 要 key、其它 403 |
| 隧道 | ⛔ **做不到**：`open-port` 只收 `http/https`、回 `tcp`，没有 UDP 映射 ⇒ peer 连不到 wg0 |
| 后端 | Router `127.0.0.1:8001`、sglang `9102`、bootstrap `9101` |

与中国移动的差别只有一处，是**平台能力**造成的：那台是「防火墙只开 UDP 51820 +
nginx 不在公网」，九章是「只开一个 TCP 30020 + nginx 就在公网上」。
所以九章这边 **API key 是唯一防线**，9101/9102 必须真的挪走。

三条规则与中国移动**逐字一致**：

- `/health` 免 key，可直接探活；
- `/v1/*` 校验 `Authorization`，`Bearer <key>` 与裸 `<key>` 都接受，**不对是 401**；
- `/metrics`、`/server_info` 及其它路径一律 **403**——403 是「路径没放行」，与 401 不同。

客户端接法：`base_url=http://120.220.102.21:30020/v1` + `Authorization: Bearer <key>`。
（中国移动那边是 `http://10.66.0.1/v1`，九章没有隧道，走平台映射的公网地址。）

⚠️ 两条九章特有的注意：

1. **`cci-edge.sh up` 仍会尝试起 wg0**，纯属为将来留门；`/dev/net/tun` 大概没有，
   就算有也没有 UDP 映射。**不要把它当隔离手段**。
2. **9001/9002 空转是这套方案的前提**，不是可选优化。这两个口关不掉，
   只能让上面没有进程。

---

## 7. 已知缺陷

### 7.1 ⛔ decode 每轮都被外部 `begin_weight_update` 打崩（不是模型/运行时问题）

**先给结论**：sglang 在九章能**完整启动**（权重加载完、PD warmup 完成、日志打出
`End of disaggregation warmup`），坏的是**暴露面**——9002 在公网上，端口一开就有人调
`/begin_weight_update`，把 8 个 rank 全打崩。

现象：pd3 每次 `start` 后约 17 分钟（加载 15–16 分钟 + warmup 1 分钟），8 个 rank 同时抛：

```
RuntimeError: Blocked unsafe global (builtins.getattr) during pickle deserialization
  at sglang/srt/utils/common.py:3424 find_class()
  ← SafeUnpickler.load() (common.py:3157)
  ← tp_worker.deserialize_own_rank (tp_worker.py:198)
  ← weight_updater.update_weights_from_tensor (weight_updater.py:241)
```

决定性的一秒（`2026-11-02 15:18:07`，`sglang-decode.log` 连续 12 行）：

```
INFO: 172.31.71.17:3215 - "POST /begin_weight_update HTTP/1.1" 200 OK   ← 外部，不是 172.19.x
Start of pd disaggregation warmup ...
End of disaggregation warmup                                             ← 启动其实是成功的
[TP0..TP7] Scheduler hit an exception: Blocked unsafe global (builtins.getattr)
SIGQUIT received. It usually means one child failed.
kill_process_tree called: parent_pid=948882, include_parent=True        ← sglang 自己杀整棵树
```

链条：`/begin_weight_update`（`http_server.py:1423`，`@auth_level(ADMIN_OPTIONAL)`，
**没配 key 时对任何人敞开**）→ 打开权重更新会话 → `/update_weights_from_tensor` 用
`SafeUnpickler` 反序列化 → 载荷里有 `builtins.getattr` 被拦 → 8 个 rank 全崩 →
sglang 自杀 → bootstrap 记 `exit -9` → 控制面判 `sglang process failed during startup` → 重来。

> 注意日志顺序会骗人：`The server is fired up and ready to roll!` 出现在 `SIGQUIT` **之后**，
> 是崩溃流程里打的最后一行。**看到它就以为起成功了，会被带到沟里。**

为什么停不下来：9002 是**平台系统端口，不可关闭**（§7.2），端口一开就在公网。
所以不是"起不来"，是"一起来就被打"—— 122 轮全部落在 17 分钟左右，
正好是"加载完、端口开始监听"的时刻。

| 证据 | 说明 |
| --- | --- |
| **`grep -c begin_weight_update sglang-decode.log` = 126** | 与 122 轮崩溃**一一对应**，无一轮例外 |
| 来源只有三个：`172.31.71.16 / .17 / .18` | 平台 NAT 后的公网源地址，轮换着打；同一批 IP 还在 `bootstrap.log` 里扫 `/phpinfo`、`/wiki`、TLS |
| `11-01 00:54:00` 一次 `POST /update_weights_from_tensor`（172.31.71.18） | 13 秒后进程 `-9`，循环起点同一分钟 |
| `11-02 15:18:07` 完整走完加载 + warmup | 6 秒后被 `kill_process_tree`，因为先中了 `begin_weight_update` |
| `11-01 01:11–02:20` 有 4 次 `fired up and ready` | 说明**不是必崩**，取决于扫描器有没有打中 |

修复路径（按代价排序）：

1. **给 sglang 加 `--api-key`** —— `srt/utils/auth.py` 的语义是「配了 key，NORMAL/ADMIN_OPTIONAL
   端点都要求 key」。但 `bootstrap/tai_talea_bootstrap/roles.py` 的 `ALLOWED_EXTRA_FLAGS`
   **不放行**这个 flag，且 Router 注册 worker 时也得带同一个 key。**未实施**。
2. **把 `service_port` / `bootstrap_port` 改到 9102 / 9101** —— 9001/9002 无人监听，
   公网映射打空。**已落进代码**：`deploy/alayanew/` 全套脚本、`provision/ssh_install.py`
   的 `runtime.bootstrap_port`、`internal/partners/alayanew` 的 `endpointFor`。
   **未在活实例上验证**——验证对象已在 2026-11-02 释放。
3. 别接公网（本来就是验证环境）。

**收尾没有在活实例上验证修复** —— 按用户决定，先记档再释放。

### 7.2 ⛔ 9001/9002 永久对公网开放，且关不掉 → 只能挪端口

- 平台**默认**把容器 9001/9002 发布到公网（pd2: 30086/30093，pd3: 30002/30003），
  它们出现在 `open-port/list` 的 `ports`（system 端口）里。
- `POST /cci/instance/{id}/close-port {"port":9002}` → **`{"status":100021,"message":"系统端口不可移除"}`**。
  只有自定义端口（8000/8787）能关。
- 后果：sglang 的 `/update_weights_from_tensor`（会反序列化 pickle）**没有任何鉴权地对互联网开放**，
  `bootstrap.log` 里能看到扫描流量：TLS 乱码、`/phpinfo`、`/wiki`、RDP cookie `mstshash=Administr`。

**结论：不要试图关端口，要让那两个口上没有进程。** 这也是 §6.5 那套门面的前提：

| 端口 | 旧 | 新 | 谁在用 |
| --- | --- | --- | --- |
| 9001 | bootstrap | **9101** | 控制面下发 start/stop |
| 9002 | sglang | **9102** | Router → worker |
| 8000 | Router（裸奔） | **门面 nginx** | 公网唯一入口，校验 key |
| 8001 | — | Router | 只监听 `127.0.0.1` |
| 8787 | 控制面（公网） | 控制面 | 只监听容器内，`close-port` 掉映射 |

两条被否掉的替代方案，记在这里省得再试：

- **iptables 兜底**：容器里 `iptables: command not found`，这条路不存在。
- **`--api-key` 挡 `begin_weight_update`**：能挡（`srt/utils/auth.py` 的 NORMAL/ADMIN_OPTIONAL
  语义是「配了 key 就要求 key」），但要同时改 bootstrap 的 flag 白名单和 Router 的 worker 注册，
  比挪端口贵。除非将来必须保留 9002，否则不用它。

### 7.3 自动扩容仍然是断的

| 段 | 状态 |
| --- | --- |
| 决策扩容 | ✗ `capacity_actions=false` + `planner_mode=fixed`（`fixed` 禁止 capacity 动作） |
| 等 instanceId | ✓ 已修（`0.9.1-cci` 换到 `/subscription/sku/cci/result/`） |
| 铺装 | ✓ 已修（NAS runtime 分支，`ssh_install.py` 只传脚本） |
| 节点自检 | ✗ 仓库默认 0.5.20/24.04/cu13，现场 0.5.22/22.04/cu12.9 |
| 公网开端口 | ✗ 全仓只有 `open-port/list`（读），没有开端口调用 |

⇒ **「把节点交进来之后」全自动，「把节点搞到手」全手工**（`2026-10-27` review 的结论没变）。

### 7.4 其它

- 控制面 / Router **无守护**：进程挂了不会自拉；容器重启清 overlay 后要手工重铺。
- `bootstrap.log` 无轮转：一天 100 MB+，`2026-11-02` 已 111 MB。
- `/health_generate` 恒 1.0s，是 sglang 代码里的 `sleep(1)`，不是性能问题。
- 弹性 GPU 池取不到卡（CPU 22 秒能落，GPU 1 卡/8 卡都 300 秒不动），已取证。

---

## 8. 看到不要慌（8 条）

1. **`/health_generate` 花 1.0 秒** —— 代码里的固定 sleep，正常。
2. **显存只占 40 GB** —— 还在加载；加载完是 73.5/80。
3. **控制面 `service_state` 卡在 `STARTING`** —— 先看 `ps` 里 sglang 起来多久；<20 分钟属正常。
4. **`bootstrap.log` 里成片的 401/400/乱码** —— 公网扫描器打 9001/9002，不是我们的客户端。
5. **`Instance Lost` / `AlertPartnerSyncFailed`** —— pull 快照里没看到实例只告警、**不释放**节点。
6. **`QUEUED` 不等于失败** —— `AwaitSSHAccess` 在 QUEUED 时停表，`ErrAwaitingCapacity` 会让 job hold。
7. **`40010 已达上限：3`** —— 配额是**实例个数**，不是卡数；`QUEUED` 的实例也占配额。
8. **`pkill -x sgl-model-gateway` 没反应** —— 名字被截断成 15 字符，用 `ps | grep '[s]gl-model-gateway launch'`。

---

## 9. 交接清单

| 东西 | 在哪 |
| --- | --- |
| 平台 AK/SK | 控制面配置 `/etc/tai-talea/tai-talea.yaml`（`partners[0].alayanew`）；**不在仓库** |
| 控制面 admin token | 同上（`server.admin_token`）；也不在仓库 |
| 实例 SSH 密码 | `POST /cci/instance/{id}/get-ssh` 现取，每次可能不同 |
| 权重 | NAS `c1efadd1-…` → `/root/gongji/GLM-5.3-Flash`（306 GB，随 NAS 保留，**不随实例释放而丢失**） |
| 运行时制品 | NAS `/root/gongji/tai-talea-runtime/`：`venv-0.5.22-cu130.tar`(11.2G)、`tai-talea-bits.tar`、`jit-caches.tar`、`SHA256SUMS`、**`live-2026-11-02.tar.gz`**（释放前的现役二进制+配置） |
| 验收报告 | `docs/alayanew-1p1d-acceptance-2026-10-26.md` |
| 容量/配额取证 | `docs/alayanew-capacity-probe-2026-10-26.md` |
| 自动化 review | `docs/alayanew-automation-review-2026-10-27.md` |
| 运行时与私有镜像 | `docs/alayanew-runtime-and-private-image-2026-10-26.md` |
| 现场脚本 | `deploy/alayanew/`（**已入库**）：`ops/` 铺装·配置·起控制面·探测，`edge/` 对外门面 |
| 对外门面 | `deploy/alayanew/edge/`：nginx 配置模板 + `cci-edge.sh`（probe/up/down/status） |

> 私有镜像**打不出来**：`save-image` 返回 `100045 请求镜像仓库失败`，租户没开通「混闪-镜像」。
> 所以重建只能走 NAS tar 这条路，不要指望镜像。

---

## 10. 还没做的（按优先级）

1. **把 §6.5 的门面真正跑一次** —— 脚本与配置已入库（`deploy/alayanew/`），
   但**没有在九章上实跑过**（实例已释放）。重建后第一件事就是
   `edge/cci-edge.sh probe` → `up` → `status`，重点看两件：
   - 9001/9002 是否真的空转（`ss -lnt | grep -E ':(9001|9002) '` 无输出）；
   - 不带 key 打 `/v1/models` 是否 401、带 key 是否 200。
2. **控制面自启** —— 现在靠手工 `setsid nohup`；写个 systemd unit 或 cron 都行。
3. **日志轮转** —— `bootstrap.log` 一天 100 MB+。
4. 自动扩容的剩下两段（§7.3）—— 只有在真要无人值守时才值得做。

---

## 11. 时间线（事实）

| 时间 | 事 |
| --- | --- |
| 2026-10-13 10:33 | pd2 创建 |
| 2026-10-13 22:04 | pd2 曾 stop 一次（overlay 清空，证明 N3） |
| 2026-10-26 17:06 | pd3 创建 |
| 2026-10-26 18:29–18:53 | 手工起 prefill 成功（加载 17 分 14 秒） |
| 2026-10-26 19:32 | **首次** `Blocked unsafe global` 异常（启动阶段） |
| 2026-10-26 20:00 | 1P1D 端到端验收：`7*8=56`，0.72s |
| 2026-10-26 20:40 | 公网 API 打通（8000→30020、8787→30021） |
| 2026-10-27 15:10 | 控制面 `0.9.1-cci` 上线（WaitCreation 修好） |
| 2026-11-01 00:54:00 | 外部 `POST /update_weights_from_tensor`（172.31.71.18） |
| 2026-11-01 00:54:19 | **重启循环开始** |
| 2026-11-02 15:18:07 | 该轮**完整启动成功**（加载 + PD warmup 全过），6 秒后被外部 `begin_weight_update` 打崩 |
| 2026-11-02 15:23 | 现役二进制与配置备份到 NAS：`live-2026-11-02.tar.gz`（39 MB） |
| 2026-11-02 15:26 | **两台实例 release**，实例列表清空 |

---

## 12. 收尾做了什么

| 动作 | 结果 |
| --- | --- |
| 只读盘点两台实例 | 进程、GPU、日志、端口、NAS 制品全部核对；**未改动任何服务** |
| 定位 decode 循环根因 | 126 次外部 `POST /begin_weight_update` ↔ 122 轮崩溃，一一对应（§7.1） |
| 尝试关 9002 公网映射 | **失败**：`{"status":100021,"message":"系统端口不可移除"}`（§7.2） |
| 备份现役件到 NAS | `/root/gongji/tai-talea-runtime/live-2026-11-02.tar.gz`，39,343,801 B，sha256 `2a648d51…61173`，含 `root/tai-talea`(0.9.1-cci)、`root/sgl-model-gateway`、`etc/tai-talea/*`（配置 + profile + bootstrap-token + onboarding） |
| 释放实例 | `glm53-pd3` 15:25、`glm53-pd2` 15:26，均 `HTTP 200 {"status":200,"message":"OK"}`；列表回到 0 条 |
| **对外方案改成与中国移动同一套** | 新增 `deploy/alayanew/edge/`（nginx 门面）与 `ops/` 全套脚本；端口规划改为 9101/9102/8001。**隧道层搬不过来**——平台只发布 TCP，见 §6.5 |
| **CCI 脚本入仓** | `deploy/alayanew/`：`ops/`（铺装、配置、起控制面、弹性探测、sweep）+ `edge/`；**凭据全部改为环境变量**，仓库是公开的 |
| **端口参数化** | `provision/ssh_install.py` 的 `runtime.bootstrap_port/service_port`（默认 9001/9002）、`provision/install_node.py` 同步读它；`internal/partners/alayanew` 新增 `endpointFor`，让平台未发布但 `advertise_hosts` 已知的端口也能纳管 |
| 代码 | 本文档 `8188fee` + `8f49fb2`；门面与脚本 `2d0201b`；端口参数化 `025bdb4` + `03af828`（adapter）；均已推 `origin/main` |

**没做的事**（下次接手时按 §10 的顺序补）：没在九章上实跑门面（实例已释放）、
没做控制面自启、没动日志轮转。

**重建时多一步**：`cci-unpack-runtime.sh` 只认 10-26 的三个 tar；现役件在
`live-2026-11-02.tar.gz` 里，解包用 `tar -xzf live-2026-11-02.tar.gz -C /`，
解完 `chmod +x /root/tai-talea /root/sgl-model-gateway`。配置里的 `advertise_hosts`
必须换成新实例的内网 IP，否则控制面探不到 worker。
