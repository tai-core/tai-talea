# A2 真机验收 + 清卡记录（2026-10-10）

本文覆盖两件事：**清掉 h100-04 上占卡的第三方容器**（用户授权），以及**在真卡上把 A2 的
参数下发链路从「派发」验到「SGLang 实际跑起来」**。最后记录新暴露的 C1 阻塞——它不是代码问题。

---

## 1. 清卡（`sgl_0521`）

### 1.1 为什么是它，且只有它

`nvidia-smi` 的 8 个计算进程全部属于同一个容器：

```
63540 / 81559 MiB (每卡)   ← 清前
295039 sglang::scheduler_TP0_EP0  63530 MiB
…      sglang::scheduler_TP7_EP7  63336 MiB
```

宿主上还有 `glm-chy`、`eval-chy`、`eval-wzj` 三个同样 running 的 vllm 容器，但**它们不在
GPU 计算进程列表里**（`hostPID: true` 的探针已确认看得到全宿主 PID），所以清一个即可。

| 项 | 值 |
| --- | --- |
| 容器 | `sgl_0521`（id `38c97da8a869…`） |
| 镜像 | `vllmascend-789c756e.ecis.zs-shanghai-1.cmecloud.cn/dev/sglang:v0.5.21-cu130`（上游 build，非内部 fork） |
| entrypoint / cmd | `/opt/nvidia/nvidia_entrypoint.sh` / `/bin/bash` |
| **restart policy** | **`no`** ← 关键：`docker stop` 不会被自动拉起 |
| 网络 / IPC | host / host |
| 挂载 | `/data01:/data01`，设备 `/dev/infiniband` |
| 启动时间 | 2026-10-09T13:28:19Z |

### 1.2 动作与结果

```bash
chroot /host docker stop -t 45 sgl_0521
```

- 结果：`Status: exited`，`ExitCode: 137`（SIGTERM 45 s 超时后 SIGKILL），**容器保留**
- 显存：8 张卡全部 `0 MiB / 81559 MiB`，计算进程列表为空
- **可回滚**：`chroot /host docker start sgl_0521` 即可原样拉起
  （restart policy 为 `no`，被显式停止的容器不会被自动重启）

### 1.3 手法

无 exec 权限的集群里，「清宿主容器」也要靠 Pod：

- 起一个 `privileged: true` + `hostPath: /` → `/host` 的 Pod
- `uid=0`，且 `/host` 可写
- 宿主 docker 客户端在 `/host/usr/bin/docker`（也有 `/host/bin/docker`），
  socket 在 `/host/run/docker.sock`
- **`chroot /host docker …`** —— 直接 `docker` 命令在 Pod 里没有，必须 chroot 进宿主根

只读识别（`docker ps`、`docker inspect`、`config.v2.json`）与写操作（`docker stop`）用的是同一套通道。

---

## 2. A2 真机验收：参数到了 SGLang 手里，且真的跑起来了

### 2.1 走法

生产配置（`host_network: true`、`gpu_count: 8`、`nodes` 单列 h100-04）下，
从控制面 API 发一次 provision：

```http
POST /v1/capacity/console/provision
{"partner_id":"cmcc-k8s","count":1,"name":"glm53-prod"}
```

得到的是一次**完整的**生命周期：Pod 建立 → bootstrap 起服务 → SGLang 加载 306 GB 权重 → 健康。

### 2.2 证据一：控制面派发日志（新增的观测点）

```
{"msg":"container enrolled","instance":"glm53-prod","endpoint":"http://10.214.81.70:9001",
 "service_endpoint":"http://10.214.81.70:9002"}
{"msg":"instance prepared","instance_id":"glm53-prod","endpoint":"http://10.214.81.70:9001"}
{"msg":"dispatching start to the bootstrap","instance_id":"glm53-prod","role":"decode",
 "extra_args":"… --mem-fraction-static=0.85 … --chunked-prefill-size=512 …
               --max-running-requests=128 --cuda-graph-max-bs-decode=128
               --disaggregation-ib-device=mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_5,mlx5_6,mlx5_7,mlx5_8"}
```

（`extra_args` 是 v0.3 新增的日志行；此前控制面把 `/bootstrap/start` 的响应体丢弃，
两侧都不留痕。）

### 2.3 证据二：容器内**实际执行**的 argv（决定性）

从同节点的探针 Pod 请求 worker 的 `/bootstrap/status`（worker 走 hostNetwork，9001 即节点 9001）：

```json
{
 "phase": "RUNNING",
 "role": "decode",
 "service_pid": 964,
 "environment": {"os":"ubuntu-24.04","cuda":"13.0","python":"3.12",
                 "sglang":"0.5.20","wheelhouse":"/opt/tai-talea/wheelhouse",
                 "virtualenv":"/opt/tai-talea/venv"},
 "history": [{"phase":"STARTING","detail":
   "/opt/tai-talea/venv/bin/python -m sglang.launch_server --model-path /models/GLM-5.3-Flash \
    --host 0.0.0.0 --port 9002 --disaggregation-mode decode --disaggregation-bootstrap-port=8998 \
    --tp-size=8 --ep-size=8 --kv-cache-dtype=bfloat16 --dsa-prefill-backend=tilelang \
    --dsa-decode-backend=tilelang --moe-runner-backend=deep_gemm --disable-shared-experts-fusion \
    --attention-backend=dsa --reasoning-parser=glm45 --tool-call-parser=glm47 \
    --served-model-name=glm-5.3-flash --disable-custom-all-reduce --disable-flashinfer-autotune \
    --mem-fraction-static=0.85 --load-format=runai_streamer --chunked-prefill-size=512 \
    --enable-metrics --enable-cache-report --chat-template=/models/GLM-5.3-Flash/chat_template.jinja \
    --max-running-requests=128 --cuda-graph-max-bs-decode=128 \
    --disaggregation-ib-device=mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_5,mlx5_6,mlx5_7,mlx5_8"}]
}
```

要点：

- `environment` 四元组与 `image_profile` 一致 → **未触发 exit 29**
- 命令行里同时出现 base、role 独有的 decode 参数、节点绑定的 device 列表 —— 三者合并正确
- 这是**容器内真实 argv**，不是控制面「打算发什么」

### 2.4 证据三：SGLang 真的起来了

```
/bOOTstrap/health -> {"status":"ok","detail":"sglang answered the health probe","phase":"RUNNING"}
```

同时宿主 `nvidia-smi` 显示 8 张卡各 **70.5 GiB / 81.5 GiB** —— 正是
`--mem-fraction-static=0.85` 的 KV 池（0.85 × 81.5 ≈ 69.3 GiB + 开销），
8 个 `sglang::scheduler_TPx_EPx` 全部在位。

**A2 的验收缺口在此闭合**：此前只验到「参数被派发」，现在验到「参数落进 SGLang 的 argv 并成功起服务」。

### 2.5 回收路径也顺带验了

```http
POST /v1/capacity/console/instances/glm53-prod/reclaim  {"grace_seconds":0}
```

```
router worker already absent; skipping readiness close   ← 缺就绪记录时不报错
service draining (grace 0s, pending_release true)
drain load probe failed: router could not read the load of http://10.214.81.70:9002
alert drain_timeout (in_flight_load 0, load_known false)
service drained forced=true exit_code=0
instance released
```

drain 的负载探针读不到负载 → 走 `drain_timeout` 强制停。根因同 §3：路由器类型不对。

---

## 3. C1 的硬阻塞：集群里的 Router 不是 `readiness-v1` 要的那一款

### 3.1 现场

控制面在 SGLang 就绪的**同一秒**进入注册/就绪步骤，然后失败：

```
WARN  service_start_failed  "router readiness failed"  detail_attempt=1 detail_max=3
                              detail_error="router worker not found: 9d717cde-4e65-490a-a194-55234a0d2a2b"
WARN  planner assign failed "router readiness failed: router worker not found: 9d717cde-…"
ERROR reconcile instance failed "router readiness failed during reconcile: router worker not found: 9d717cde-…"
```

（之后每 10 s 复现一次。）

### 3.2 根因

探针直接问路由器：

| 请求 | 结果 |
| --- | --- |
| `GET /health` | 200 `OK` |
| `GET /workers` | 200 —— 有列表（含 23 h 前手工 1P1D 的 `http://10.214.81.70:30000`） |
| **`GET /state/workers`** | **404 Not Found** ← readiness-v1 的「就绪面」不存在 |
| `GET /get_server_info` | 200 `workers_count: 3` |
| `GET /v1/models` | 200 |

- 集群里跑的是**旧版 Python `sglang_router`**（`python3 -m sglang_router.launch_router`，
  即 23 h 前手工 1P1D 留下的 Job `glm53-pd-router`）
- `/state/workers` 是 **Rust `sglang-router`（crate `smg` / `sgl-model-gateway`）** 的管理面：
  `sglang-router/src/server.rs:860` 注册了 `.route("/state/workers", get(list_worker_readiness))`
- 控制面**只支持 `readiness-v1`** 一种模式：
  `routeradapter/client.go:94` `if c.EffectiveMode() != ModeReadinessV1 { return error }`

所以：注册（`POST /workers`）能过，就绪（`/state/workers/{id}`）必挂 → `serving` 永远到不了。

### 3.3 为什么不是「改个开关就好」

- 没有第二个 mode 可切（源码硬校验）
- 就绪是**唯一**的摘流原语：`lifecycle.go:662` 「Close readiness first: this is the only
  supported drain primitive」
- 要跑 C1 必须把 Rust `sglang-router` 真的部署进集群

### 3.4 交付 Rust router 的三条路（未实施，供决策）

| 路线 | 说明 | 主要障碍 |
| --- | --- | --- |
| A. 构建 smg 静态二进制 → 自造镜像 → 推 ecis | 复用已有「无 docker OCI 构建 + Registry v2 推送」管线（talea-control/worker 镜像就是这样做出来的） | 需要一个能跑 Rust + protoc 的构建环境；本机无 Rust/docker |
| B. 在 n1 上装工具链直接编 | n1 有 gcc/make/curl/apt（Ubuntu 26.04，4 核 7 GB，/ 余 86 GB） | 在k8s 节点上装 cargo/protobuf-compiler 属侵入式；4 核 7 GB 编 vendored-openssl 项目偏小 |
| C. 写兼容 shim 代理 | 在集群里放一个小代理：`/workers*` 转发给 Python 路由器，`/state/workers*` 落到本地就绪表 | C1 验的是「控制面与路由器的就绪契约」，shim 等于把被验对象换掉了；只适合做临时验证 |

`smg` 的 Dockerfile 是 `rust:1-bookworm` + `build-essential cmake libprotobuf-dev protobuf-compiler
pkg-config`，`cargo build --release --features vendored-openssl --bin sgl-model-gateway`。

---

## 4. 本次之后集群的状态

| 对象 | 状态 |
| --- | --- |
| `tai-talea`（控制面 v0.3） | 1/1 Running |
| `talea-worker-service` | 1/1 Running |
| `glm53-pd-router`（旧 Python 路由器） | 1/1 Running，23 h |
| `glm53-pd-{prefill,decode}` | Completed（23 h 前手工 1P1D 残留） |
| 全部探针 / worker Pod | 已删除 |
| **h100-04 显存** | **8 × 0 MiB** |
| `sgl_0521` | exited(137)，容器保留，可 `docker start` 回滚 |

---

## 5. 结论与下一步

- **A2 完成**：代码 + 模板 + 单测 + **真机端到端**（参数落到 SGLang 的 argv 并健康）
- **C1 未完成，且阻塞不在本项目代码**：需要把 Rust `sglang-router` 部署进集群
- 其余前置未变：h100-01 缺 ecis `/etc/hosts` 映射（平台）；控制面单副本绑 h100-04

**顺序建议**：部署 Rust router（C1）→ h100-01 的 hosts 映射（平台）→ 1P1D 端到端 → D1/D2。
