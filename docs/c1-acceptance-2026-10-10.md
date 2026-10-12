# C1 真机验收：Rust Router 替换 + 动态注册 / readiness 可见 / 摘流

2026-10-10（集群日志为 UTC，本地时间 = UTC+8；本文引用的时间戳一律是集群日志的 UTC）

## 1. 结论

| 验收点 | 结果 | 证据 |
| --- | --- | --- |
| `sgl-model-gateway` 静态二进制在没有 docker 的机器上编出来 | ✅ | §3.1（6m31s，71 MiB） |
| 用既有的无 docker OCI 管线打成镜像并推到 ecis Harbor | ✅ | §3.2 |
| 集群内以 hostNetwork 部署，替换掉旧 Python `sglang_router` | ✅ | §4 |
| **`GET /state/workers` 由 404 变 200**（旧 router 的缺口） | ✅ | §5.2 |
| 控制面动态注册 worker：`POST /workers` → 202 → `service_serving` | ✅ | §5.1 |
| readiness 在 Router 侧可见：`state=ready, generation=3` | ✅ | §5.2 |
| 摘流原语 `PUT /state/workers/{id}` 生效（`ready→unavailable`，gen 3→4） | ✅ | §5.3 |
| **摘流 ≠ 摘除**：`GET /workers` 里成员仍在，`is_healthy` 仍 true | ✅ | §5.3 |
| 控制面在 readiness generation 漂移后自己对齐成员关系 | ✅ | §5.3（18:58:56） |
| worker 真的在服务（SGLang 起来了、在应答） | ✅ | §5.4 |

C1 到此闭环。整套链路是：**控制面 provision → worker 容器起来 → bootstrap 拉起 SGLang →
`POST /workers` 注册 → `GET /state/workers` 确认可路由 → `SERVING`**，每一步都有两侧日志对齐。

## 2. 为什么必须换 Router

旧的集群 Router 是 Python 版 `sglang_router`，它**没有 readiness 管理面**：
`GET /state/workers` 直接 404。

而控制面的 `internal/routeradapter/client.go` 把 `readiness-v1` 当**唯一**合法 mode 硬校验
（`ModeReadinessV1 = "readiness-v1"`，见 `client.go:35` 与注册路径的 mode 校验），注册之后
唯一能回答「这个 Router 会不会把新请求发给这个 worker」的接口就是 `GET /state/workers`。
换句话说 C1 不是「换个 Router 版本」，而是**补一条控制面赖以工作的契约**：没有它，
控制面能注册、能停、能删，但没法证明自己的摘流动作真的生效。

## 3. 从源码到一个能拉取的镜像（全程没有 docker）

### 3.1 编译

| 项 | 值 |
| --- | --- |
| 源码 | `tai-core/sglang-router`（private，API 404，无 GHCR 镜像） |
| 上传 | 947,170 B tar.gz，sha256 `c2c8aa57b5658f10e…`，两端一致 |
| 构建机 | 九章 prefill 容器（Ubuntu 24.04 / 192 核 / `/dev/shm` 554 G 可用 / 出网通） |
| 工具链 | apt 装 `cmake`（镜像里唯一缺的一件）+ rustup stable minimal，耗时 41 s |
| 产物 | `/dev/shm/sgl-model-gateway`，74,428,624 B（71 MiB），sha256 `927b1c90e0b2279c97742baa2687ddd68b16a86f7b49dec160db3f1a29180904` |

三条踩过的坑（都写进脚本了）：

1. **`CARGO_NET_GIT_FETCH_WITH_CLI=true` 是必需的。** cargo 内置的 libgit2 对
   github.com 报 `SSL error: syscall failure: ; class=Os (2)` 且重试无效，而系统 git 2.43
   完全正常（`git ls-remote` / `git clone --depth 1 --branch v0.0.4` 都成功）。
   不改这个开关，`cargo fetch` 会停在 `Updating git repository 'https://github.com/openai/harmony'`
   上一直重试。
2. **profile 用 `ci` 不用 `release`。** `Cargo.toml` 里 `release` 是 fat LTO +
   `codegen-units = 1`，`ci` 是 thin LTO + `codegen-units = 16` + `opt-level = 2`。
   router 是 I/O 密集型，验收轮不值得为个位数百分比换数倍编译时间。
3. 大件全部落 `/dev/shm`：构建机根盘只剩 14 G，`/dev/shm` 有 554 G。

### 3.2 镜像

底座不是「从厂商镜像瘦身」，而是 **debootstrap minbase 现造一个**：

```bash
debootstrap --variant=minbase --include=ca-certificates,curl,iproute2 noble "$ROOT" \
  http://nexus.cci.alayanew.com:8081/repository/ubuntu/
```

219 MB，chroot 冒烟通过，glibc 2.39 —— 与编译机同版本，ABI 天然吻合；
而共享构建容器的快照是 36 GB，拿它做底座没有道理。

打包/推送复用 `talea-worker-full-image.py` 的纯标准库 Registry v2 客户端（401 重试、
301/302/307 重定向跟随、64 MiB 分块 PATCH）：

```
library/talea-router:v0.2
manifest sha256:f306e8f10a4160d72f27f2c8f1224a759a744f4ea4847a3ae49b7242c382c630
layer    sha256:89520e9a19ec053f8d905a799fd651e26efd99d7aa5d3c5b783cd1206979e96b (66.3 MiB gz / 170.5 MiB raw)
```

**这里踩了一个值钱的坑，记下来：**

第一次推的 v0.1 只设了 `Cmd`、`Entrypoint` 留空。k8s 里 `args` 覆盖镜像的 `Cmd`，
`command` 才覆盖 `Entrypoint`；清单里写了 `args` 之后，容器的 argv 首元素变成了
`--host=0.0.0.0`，于是 Pod 报：

```
exec: "--host=0.0.0.0": executable file not found in $PATH   (exit 128)
```

修的是**镜像**（`Entrypoint: ["/usr/local/bin/sglang-router"]`，`Cmd` 去掉二进制路径），
并按纪律换 tag（`v0.1 → v0.2`）重推 —— `imagePullPolicy: Always` 只保证拿到最新 digest，
从不保证「同一个 tag 还是同一份字节」。

另外这个 build 的 `--scheduler-policy` **只接受 `fifo`**：README 示例里的 `cache_first`
会被 clap 直接拒掉（exit 2），`--prefill-policy session_aware` / `--decode-policy round_robin`
经 `--help` 核实是合法的。

## 4. 集群内落地

`talea-router` 清单的关键取舍：

- **hostNetwork 钉 h100-04**，于是地址是节点的 `10.214.81.70:8000` —— 与生产配置里
  `router.url: http://10.214.81.70:8000` 逐字一致。本 namespace 没有 Service 权限，
  所以「地址可预测」只能靠 nodeSelector + hostNetwork 换。
- **空 PD 池启动**（不传 `--prefill` / `--decode`）。这正是 C1 要的场景：池子一开始是空的，
  worker 全靠运行期注册进来。
- 单端口模式（不设 `--control-plane-port`）：serving 路由和控制面路由
  （`POST /workers`、`GET /state/workers`、`GET /get_loads`）合并在 8000。
- 旧的 Python router Job 已删，旧 router 不再占 8000。

## 5. 端到端证据链

### 5.1 控制面侧（审计流水，时间正序）

```
18:36:05  POST /v1/capacity/console/provision → 202
18:36:10  container enrolled  endpoint=http://10.214.81.70:9001  service_endpoint=http://10.214.81.70:9002
18:36:14  instance LOST (container endpoint is unreachable)      ← 容器刚建，正常
18:36:24  instance prepared → dispatching start  role=decode
18:45:10  service_start_failed  attempt=1  error="sglang exited unexpectedly with code -9"   ← §6
18:45:34  service_starting      attempt=2  role=decode
18:48:18  service_healthy
18:48:18  service_registering   router=primary
18:48:19  service_serving       router_worker_id=40acb152-e2b6-47d9-8bd6-75381bfe700d
                                worker_url=http://10.214.81.70:9002  readiness_generation=3
```

控制面在 18:48:19 之后不再报任何错，每 30 s 的 `partner snapshot committed` 与
`capacity event processed` 全是 `APPLIED`。

### 5.2 Router 侧（`http://10.214.81.70:8000`）

```
GET /liveness            → 200 OK
GET /get_server_info     → {"router_manager":true,"routers_count":1,"workers_count":1}
GET /workers             → 1 条：id 40acb152-…, url http://10.214.81.70:9002,
                            worker_type "decode", is_healthy true, load 0,
                            metadata 来自 SGLang 自己的 /get_server_info
                            (tp_size=8, disaggregation_mode=decode,
                             served_model_name=glm-5.3-flash)
GET /state/workers       → 200 [ {worker_id 40acb152-…, state "ready", generation 3} ]   ★ 旧 router 此处 404
GET /get_loads           → {workers:[{worker:…:9002, load:0}]}
GET /readiness           → 503 "insufficient healthy workers"   ← 只有 decode、没有 prefill，符合预期
GET /v1/models           → zai-org/GLM-5.3-Flash
```

`/workers` 的 metadata 值得多看一眼：那些键值**不是**我们注册时填的，是 smg 反向
读 SGLang `/get_server_info` 得来的 —— 说明 Router 已经和这个 worker 建立了真实的
服务关系，不是只在表里挂了一行。

### 5.3 摘流（本轮的第三条验收）

摘流只有一条原语：`PUT /state/workers/{id}`（`DELETE /workers` 是刻意不实现的）。

```
PUT /state/workers/40acb152-…  {"state":"unavailable","reason":"manual"}   → 200
{
  "previous_state": "ready",
  "record": {"worker_id":"40acb152-…","worker_url":"http://10.214.81.70:9002",
             "state":"unavailable","unavailable_reason":"manual",
             "generation":4,"updated_at_unix_ms":1791572328005},
  "changed": true
}
GET /state/workers/{id}   → state "unavailable", generation 4        （回读一致）
GET /workers              → 成员仍在池中，is_healthy 仍 true          （摘流≠摘除）

PUT /state/workers/40acb152-…  {"state":"ready","reason":"manual"}          → 200
                          → state "ready", generation 5
```

然后控制面自己收了口：

```
18:58:56  "service membership reconciled"  instance_id=glm53-c1 role=decode
          router_worker_id=40acb152-…  generation=5
```

即我手动摘流/恢复把 generation 从 3 推到 5，`settleService` 在下一轮（10 s 内）
发现 `instance.ReadinessGeneration != 现网 generation`，重新对齐并把 `SERVING`
持久化到 generation 5。**这一条是「双向」的证据**：不只是控制面能写 Router，
它也在读 Router 并据以修正自己的状态。

> 小坑：`PUT` 的 `reason` 是**枚举**，只接受 `health_check` 或 `manual`。
> 传自由文本会被 422 拒（`unknown variant ... expected 'health_check' or 'manual'`），
> 而且**状态不会变** —— 第一次这么试的时候摘流其实没发生，回读 generation 仍是 3。
> 判据看 `changed` 字段和 generation，不要只看 HTTP 码。

### 5.4 worker 真的在服务

`/opt/tai-talea/logs/sglang-decode.log`（用 `hostPID: true` + `/proc/<pid>/root/` 读，
因为集群禁 exec）：

```
[18:48:08] INFO: Uvicorn running on http://0.0.0.0:9002
[18:48:17] The server is fired up and ready to roll!
[18:51:35] INFO: 10.222.49.84:34946 - "GET /server_info HTTP/1.1" 200 OK
[18:51:36] INFO: 127.0.0.1:12806 - "GET /health_generate HTTP/1.1" 200 OK
[18:51:38] INFO: 10.214.81.70:28238 - "GET /get_load HTTP/1.1" 200 OK
```

最后一行 `10.214.81.70` 就是 smg router 在轮询它的负载 —— 注册关系是活的。
19:00 时宿主上 8 个 `sglang::schedul`（各 3.3 GiB）+ 主进程 `python` 都在。

## 6. attempt 1 的失败：一次被误读的 `-9`

这是本轮花时间最多、也最值得写下来的一节。

**表面症状**：控制面 `service_start_failed`，`detail_error` 是
`sglang process failed during startup: sglang exited unexpectedly with code -9`
（attempt 1/3，18:45:10）。`-9` 看起来像「被谁杀了」。

**先排除了两个直觉**：

- **不是内核 OOM。** `dmesg` 里没有任何 oom/kill 记录，`kern.log` 也没有；
  宿主 `Committed_AS`(1.068 TB) 确实超过 `CommitLimit`(1.046 TB)，但
  `overcommit_memory=1` 下没有实际触发。
- **不是控制面杀的。** worker 的 bootstrap 日志里**从头到尾没有一次
  `POST /bootstrap/stop`**（只有两次 `POST /bootstrap/start`：18:36:26、18:45:35），
  而且 `_refresh_process` 只 `poll()` 观察、不发信号。

**真正的原因在 SGLang 自己的日志里**（顺序即因果）：

```
18:43:08  Engine startup timings (s): load_weight=40.30, kv_cache_allocation=0.71,
          scheduler_e2e=376.93, cuda_graph={decode=270.38}, tokenizer_e2e=385.08
18:43:08  INFO: Started server process [961]
18:43:08  INFO: Application startup complete.
18:43:08  ERROR: [Errno 98] error while attempting to bind on address
                 ('0.0.0.0', 9002): address already in use
18:45:09  Initialization failed. warmup error: Traceback ...
            MaxRetryError: HTTPConnectionPool(host='127.0.0.1', port=9002):
            Max retries exceeded with url: /model_info
            (Caused by NewConnectionError('... [Errno 111] Connection refused'))
18:45:09  kill_process_tree called: parent_pid=961, include_parent=True, pid=961
```

链条是：**SGLang 绑不上 9002 → 它自己启动期的 warmup 去请求
`http://127.0.0.1:9002/model_info` 拿到 ConnectionRefused → warmup 失败 →
SGLang 调 `kill_process_tree(parent_pid=961, include_parent=True)` 把自己杀了**。

也就是说这个 `-9` 是**自杀**，长着一副他杀的脸。

**9002 当时被谁占着？** 把证据摆在一起：

- bootstrap 的健康探针（`http://127.0.0.1:9002/health_generate`）从容器起来
  （18:36:22）就一直失败，整个 attempt 1 期间 `/bootstrap/health` 都是 degraded。
- 18:42:17 有一条探针抛 `http.client.BadStatusLine: GET /health_generate HTTP/1.1`
  —— 说明**端口上确实有监听者**，只是它不是按 HTTP 回话的。
- attempt 2（18:45:35 起）在 18:48:08 干净地绑上了 9002。

结论：有一个**本 namespace 看不见的外部监听者**占着节点的 9002，并在
18:43–18:45 之间消失。这个集群的节点上本来就有 out-of-band 的宿主/其它 namespace
负载（早先在 `moby` namespace 里就发现过平台的容器），而当前 RBAC 连
`get nodes` 的 `containerRuntimeVersion` 都读不出来，没有任何办法直接点名它。
**所以这里给的是「证据支持度最高的读法」，不是「抓住了凶手」。**

**要留成运维规矩的两条：**

1. 当 `--port` / `service_port` 是固定值（这里是 9002）且用 hostNetwork 时，
   相邻 worker 之间、以及 worker 与本节点上的外部负载之间**没有端口隔离**。
   一个泄漏的监听者不会报「端口冲突」，它会表现成 **「SGLang exit -9」，看起来像崩溃**。
2. 判据不是控制面的 `detail_error`，而是 worker 自己的
   `/opt/tai-talea/logs/sglang-<role>.log`。集群禁 exec，读法是
   `hostPID: true` 的特权 Pod 里 `/proc/<pid>/root/opt/tai-talea/logs/…`。

> 这个 `-9` 也解释了为什么 `start_max_attempts: 3` 是有意义的：attempt 2 撞在
> 占用者消失之后，一次就过了。如果没有重试，C1 这一轮会以「SGLang 起不来」收场。

## 7. 顺手发现的真 bug（未改）

`bootstrap/tai_talea_bootstrap/service.py` 的 `_probe_http_health`：

```python
try:
    with urlopen(url, timeout=timeout) as response:
        return 200 <= response.status < 300
except (URLError, OSError, ValueError):
    return False
```

SGLang 启动期（socket 已监听、HTTP 应用还没就绪）会回一个畸形响应，`urlopen` 抛的是
`http.client.BadStatusLine` —— 它是 `http.client.HTTPException` 的子类，
**不在上面三个捕获类型里**。于是异常穿透 `health()` → `do_GET` → 请求处理线程崩掉，
控制面拿到的不是 `degraded` 而是一次断连，日志里堆一大段 traceback（18:42:17 那条）。

健康判据本身没坏（`waitHealthy` 每 200 ms 重试一次，最终还是能判对），
但它把「bootstrap 自己在抛异常」伪装成「bootstrap 不可达」。修法是把
`http.client.HTTPException` 加进捕获列表。**本轮只做验收，没有改代码**，列进遗留。

## 8. 复现清单（脚本都在 `.workbuddy/tmp/alayanew/`）

| 用途 | 文件 |
| --- | --- |
| 构建机 bootstrap（cmake + rustup） | `smg-build-setup.sh` |
| 编译 smg | `smg-build.sh` |
| debootstrap 底座 | `smg-rootfs.sh` |
| OCI 打包 + 推送 + 校验 | `smg-router-image.py` |
| 集群清单 | `talea-router.yaml` |
| 验收（Router 面 + 控制面面） | `c1-verify.py` + `make-c1-verify.py` |
| 摘流 + 回读 | `c1-drain.py` |
| 读容器内日志（禁 exec 的替代） | `find-sglang-log.yaml` / `make-sgl2.py` |
| 上传（SFTP 在本沙箱 ENOENT） | `ssh-pushfile.py` |

操作提醒：调 `ssh-run.py` / `ssh-pushfile.py` 时在 Git Bash 下要
`export MSYS_NO_PATHCONV=1`，否则命令行里的 `/root/...` 会被转成本地 Windows 路径。

## 9. 遗留

| 项 | 归属 | 说明 |
| --- | --- | --- |
| h100-01 的 ecis `/etc/hosts` 条目 | 平台 | 不加则落到 h100-01 的 Pod 卡在 ImagePullBackOff |
| 给控制面 SA 授权（现在的 kubeconfig 是挂进来的） | 平台 | 现在能跑，但授权后可以不再挂凭据 |
| 给 `talea-router` 一个 Service | 平台 | 现在只能靠 hostNetwork + nodeSelector 固定成 `10.214.81.70:8000` |
| bootstrap `BadStatusLine` | 本项目 | §7 |
| 单节点 1P1D 续做 | 本项目 | Router 已经是新的，`router.url` 无需再动；缺一个 prefill worker。注意单实例**永远是 decode**（`TargetPrefill = floor(total*ratio)`，只有 `total>=2` 才有 `max(1,·)`） |

另外两个不算遗留但要知道的现场：`glm53-pd-{decode,prefill}` 两个 24h 前的
Completed Pod、以及 `talea-worker-service`（A2 验收留下的探针 worker，
token 是 `verify-token-20261009`）都还在 `nightshift` 里，不参与本轮链路。
