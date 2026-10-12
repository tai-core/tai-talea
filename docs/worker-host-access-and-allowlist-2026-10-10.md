# A1 + B1：worker Pod 的宿主访问与 bootstrap 白名单（2026-10-10）

> 关闭缺口清单里的 **A1**（adapter 缺宿主/特权开关）与 **B1**（bootstrap 参数白名单扩充）。
> 前置：M1（控制面进集群，`docs/control-plane-nightshift-2026-10-09.md`）、
> M2（worker 镜像供给，`docs/talea-worker-selfcontained-image-2026-10-09.md`）。
>
> 本轮不只是改代码：把新版控制面推进集群、用 `provision` API 真的建了一个 worker Pod，
> 于是**第一次走到了 `/bootstrap/start`**。那一步暴露出一个独立缺陷（worker 镜像里的
> bootstrap 是 2026-09-30 的旧快照），本文一并记录。

## 1. 结论

| 项 | 结果 |
| --- | --- |
| A1 代码 | ✅ `host_network / host_ipc / privileged` 三个开关，默认全关；打开时自动带 `dnsPolicy: ClusterFirstWithHostNet` |
| A1 集群验收 | ✅ 控制面 `POST /v1/capacity/console/provision` 建出的 worker Pod，spec 里 hostNetwork/hostIPC/privileged/`/dev/infiniband` 全部到位，**Pod IP 等于 h100-04 的宿主 IP** |
| B1 代码 | ✅ 白名单补 5 个旗标；`deploy/kubernetes/tai-talea.yaml` 里 fork 不接受的 `mooncake_tcp` 删除 |
| B1 生效前提 | ⚠️ 依赖 worker 镜像里的 bundle，而镜像里那一份是旧的 → **已重建镜像 `talea-worker:v0.2`** |
| B1 集群验收 | ✅ 同一条 provision 链路的失败点从「422 参数被拒」推进到「sglang 起不来」——参数确实进去了 |
| 新阻塞（非本轮代码问题） | ⛔ h100-04 的 8 张卡被一个**非 k8s 的 Docker 容器**占满（82 GB 里用掉 63.5 GiB），worker 无法启动，见 §7 |
| 配置模板 | ✅ `control-plane-nightshift-2026-10-09.md` §3 的八处修正已并入仓库模板；`tai-talea-control-plane.yaml` 同步 |

## 2. A1：adapter 的宿主访问开关

### 2.1 改了什么

| 文件 | 改动 |
| --- | --- |
| `internal/config/config.go` | `PartnerKubernetesConfig` 加 `host_network` / `host_ipc` / `privileged`（均 `bool`，默认 false） |
| `internal/partners/registry.go` | 三个字段透传到 `kubernetes.Config` |
| `internal/partners/kubernetes/adapter.go` | `Config` 加 `HostNetwork` / `HostIPC` / `Privileged` |
| `internal/partners/kubernetes/client.go` | `PodSpec` 加 `hostNetwork` / `hostIPC` / `dnsPolicy`；`SecurityContext` 加 `privileged` |
| `internal/partners/kubernetes/manifest.go` | 渲染上述字段；`HostNetwork` 打开时同时写 `dnsPolicy: ClusterFirstWithHostNet` |

默认全关是刻意的：只做 CPU worker、或自带 device plugin 的集群，不该因为 adapter 学会了这几个
开关就被塞一个特权宿主网络 Pod。`/dev/infiniband` 的挂载不需要代码改动——现有 `Volumes`
（hostPath）就能表达。

### 2.2 一个容易漏的伴生项：dnsPolicy

Kubernetes 不接受 host-network Pod 使用默认的 `ClusterFirst`。kubelet 会拒绝把 dnsPolicy 写进
Pod sandbox，然后**静默退回节点解析器**——集群内的名字全都解析不了，而 Pod 看起来是 Running 的。
所以 `HostNetwork` 一打开就必须一起写 `ClusterFirstWithHostNet`。代码里两者绑在同一个 `if` 里，
测试也断言了这一点。

### 2.3 验收证据

控制面（v0.2 镜像，跑在 h100-04）收到 `POST /v1/capacity/console/provision
{"partner_id":"cmcc-k8s","count":1,"name":"glm53-a1-probe"}` 之后建出的 worker Pod
（`kubectl get pod glm53-a1-probe -o json` 摘要）：

```json
{
  "podIP": "10.214.81.70",
  "hostIP": "10.214.81.70",          // ← 两者相等，就是 hostNetwork 生效的直接证据
  "node": "h100-04",
  "labels": {
    "app.kubernetes.io/managed-by": "tai-talea",
    "tai-talea.instance": "glm53-a1-probe",
    "tai-talea.partner": "cmcc-k8s"
  },
  "hostNetwork": true,
  "hostIPC": true,
  "dnsPolicy": "ClusterFirstWithHostNet",
  "restartPolicy": "Always",
  "grace": 300,
  "volumes": [
    {"name": "models",     "hostPath": "/apps/models"},
    {"name": "infiniband", "hostPath": "/dev/infiniband"},
    {"name": "shm",        "emptyDir": {"medium": "Memory", "sizeLimit": "512Gi"}}
  ],
  "container": {
    "image": "shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn/library/talea-worker:v0.1",
    "pull": "Always",
    "securityContext": {"privileged": true},
    "limits": {"nvidia.com/gpu": "8"},
    "mounts": [
      {"name": "models",     "mountPath": "/models",          "readOnly": true},
      {"name": "infiniband", "mountPath": "/dev/infiniband"},
      {"name": "shm",        "mountPath": "/dev/shm"}
    ],
    "env": ["TAI_TALEA_BOOTSTRAP_PORT","TAI_TALEA_BOOTSTRAP_TOKEN","TAI_TALEA_PROFILE",
            "TAI_TALEA_SERVICE_PORT","TAI_TALEA_START_WAIT_SECONDS"],
    "probe": "/bootstrap/health"
  }
}
```

四点值得单独指出：

1. **Pod IP == host IP**。这是 hostNetwork 的硬证据，不依赖任何注解。
2. `/dev/infiniband` **不是只读**。设备节点在只读挂载上无法以写方式打开，而 verbs 调用就是写——
   模板里刻意不写 `read_only`。
3. 集群的准入策略自己往 `nodeSelector` 里补了一条
   `node-restriction.kubernetes.io/tenant: nightshift`。平台行为，无害，但说明 nodeSelector
   不完全由我们控制。
4. 控制面日志同一时刻给出：

```
{"msg":"instance prepared","instance_id":"glm53-a1-probe","endpoint":"http://10.214.81.70:9001","partner_id":"cmcc-k8s"}
```

即 provision → 建 Pod → waitRunning → `EnrollPrepared` 整条链都通了。

## 3. B1：bootstrap 白名单与配置

### 3.1 白名单

`bootstrap/tai_talea_bootstrap/roles.py` 新增 5 个旗标，每一个都有实证理由：

| 旗标 | 理由 |
| --- | --- |
| `--disaggregation-ib-device` | 1P1D 的 KV 传输绕开坏卡靠它（h100-01 的 mlx5_6 路径不通，剔除后 PD 才通） |
| `--cuda-graph-max-bs-decode` | 0.5.20 把聚合的 `--cuda-graph-max-bs` 拆成按角色的两个，decode 那个用来压 graph 显存 |
| `--disable-shared-experts-fusion` | deep_gemm MoE runner 不走这条融合路径，开着会改变数值路径 |
| `--enable-cache-report` | Router 的 readiness 视图建在它上面；没有它，一个在服务的 worker 仍可能被算作不可用 |
| `--served-model-name` | prefill/decode 必须报同一个名字，否则 Router 会当成两个模型 |

同时把 `--disaggregation-transfer-backend` 的注释改成「不要下发」：白名单匹配的是**旗标**，
不是取值，所以这个旗标留着无害，但内部 fork 硬编码 RDMA、写 `mooncake_tcp` 会被 sglang 直接拒。

### 3.2 配置模板

`deploy/kubernetes/tai-talea.yaml` 删掉了 `--disaggregation-transfer-backend=mooncake_tcp`，
并把 `control-plane-nightshift-2026-10-09.md` §3 的八处修正正式并入模板（之前只存在于临时副本）：

| # | 项 | 值 |
| --- | --- | --- |
| 1 | `router.url` | `http://10.214.81.70:8000` |
| 2 | `kubernetes.image` | ecis 的 `library/talea-worker:v0.1` |
| 3 | `image_pull_policy` | `Always` |
| 4 | `node_selector` | `kubernetes.io/hostname: h100-04` |
| 5 | `profile_path` | `/opt/tai-talea/profile.json`（错则 exit 29） |
| 6 | `extra_sglang_args` | 删 `mooncake_tcp`，补 `--attention-backend=dsa` / 两个 parser / `--served-model-name` / `--enable-cache-report` / `--disable-shared-experts-fusion` / `--enable-metrics` |
| 7 | `run_as_user` / `run_as_group` | 删除 |
| 8 | `kubeconfig` 挂载点 | `/etc/tai-talea-kubeconfig`（与 config 目录平级） |

模板里新增的部分：`host_network / host_ipc / privileged: true`、`/dev/infiniband` 卷。
`--max-running-requests` / `--cuda-graph-max-bs-decode` / `--chunked-prefill-size` 三个**按角色不同**
的参数刻意不放进共享列表——那是 A2 的活。

配置里仍然只有**占位** token。真实 token 只存在于节点的 hostPath 配置里（见 §5）。

### 3.3 测试

- `internal/config/config_test.go` 新增 `TestKubernetesConfigurationIsValid`：把模板当契约测
  ——host 三开关必须为真、pull policy 必须 `Always`、`profile_path` 必须是镜像内路径、
  `run_as_user/group` 必须为空、extra args 不得含 `mooncake_tcp`、`/dev/infiniband` 必须挂上且不能只读。
- `internal/partners/kubernetes/adapter_test.go` 新增
  `TestBuildPodGrantsHostAccessOnlyWhenConfigured`：默认不打开；打开时三个字段 + dnsPolicy 都在；
  `runAsGroup` 仍不被覆盖。
- `bootstrap/tests/test_bootstrap.py` 新增 `test_the_h100_worker_flag_set_is_allowed`；
  原来那条拿 `mooncake_tcp` 当示例值的测试改成 `mooncake`，并注明取值不是白名单的事。

全量：`go test ./internal/...` 全绿；`python -m unittest discover -s bootstrap/tests` 69 项通过
（2 项在非 Linux 上按设计跳过）。

## 4. 首次真实启动暴露的缺陷：worker 镜像里的 bundle 过期

### 4.1 症状

provision 成功后 2 秒，控制面日志：

```
{"msg":"alert","alert":"service_start_failed","instance_id":"glm53-a1-probe",
 "message":"bootstrap start failed",
 "detail_error":"bootstrap returned HTTP 422: {\"accepted\": false, \"phase\": \"FAILED\",
  \"detail\": \"extra argument '--ep-size' is not allowed\", \"exit_code\": 64}"}
{"msg":"planner assign failed","instance_id":"glm53-a1-probe","role":"decode", ...}
```

`--ep-size` **在仓库的白名单里**，却被 worker 拒了。所以问题不在控制面，在 worker 自己带的那份代码。

### 4.2 定位：无 exec 的集群里怎么读镜像内的文件

这个命名空间禁 `exec`，但允许建 Pod 且允许读日志。于是用**同一个镜像**起一个 Pod、只覆盖
`command`，让容器把文件打到 stdout：

```yaml
image: .../library/talea-worker:v0.1
imagePullPolicy: Never
command: ["/bin/bash","-lc"]
args: ["sha256sum /opt/tai-talea/tai_talea_bootstrap/roles.py; sed -n '18,80p' /opt/tai-talea/tai_talea_bootstrap/roles.py"]
```

结果：镜像内 `roles.py` 的 sha256 是 `a9336ac0…`，白名单**只有 19 个旗标**，里面
**没有** `--ep-size`、没有 `--load-format`、也没有 `--model-loader-extra-config`。

### 4.3 根因

worker 镜像是在九章 prefill 容器上、从该容器既有的 `/opt/tai-talea` 树整盘打的快照。那棵树里
`tai_talea_bootstrap/` 停在 **2026-09-30 12:27**（`roles.py` 5035 字节），此后再没同步过。
镜像忠实反映了它，所以镜像里的 bundle 比仓库落后了整整一段：

| | 镜像内（2026-09-30） | 仓库（2026-10-10） |
| --- | --- | --- |
| `roles.py` sha256 | `a9336ac0…` | `05421c2d…` |
| 白名单旗标数 | 19 | 35 |
| `--ep-size` | ✗ | ✓ |
| `--chat-template` | ✗ | ✓ |
| `--load-format` | ✗ | ✓ |
| `--disaggregation-ib-device` | ✗ | ✓ |
| `--served-model-name` / `--enable-cache-report` | ✗ | ✓ |

影响面比想象的宽：配置里的 `--load-format=runai_streamer` 和 `--chat-template=…` 同样会被拒。
也就是说，**在此之前 worker 从来没有真正走到过 sglang 启动**——`talea-worker-service` 当时
1/1 Running 只证明 bootstrap 起来了并答了健康检查，不代表能 start。这个缺陷被 M2 的验证
盲区藏住了：M2 验的是「镜像能被拉起来」，不是「镜像能开工」。

### 4.4 修复

1. 把仓库当前的 `bootstrap/tai_talea_bootstrap/`（8 个 `.py`）同步进九章容器的 `/opt/tai-talea/`，
   旧树留作 `tai_talea_bootstrap.bak-20260930` 以便对比（对比完即删，免得混进镜像）。
2. 容器上自检：白名单 35 项、接受完整 worker 旗标集、仍然拒绝 `--api-key`。
3. 用同一套无 docker 管线重打镜像并推送为 **`library/talea-worker:v0.2`**
   （脚本 `/opt/talea-worker-full-image.py`，`TALEA_TAG` 可指定 tag）：

   | 项 | 值 |
   | --- | --- |
   | manifest | `sha256:9cdf31b5d2c8ec40d138a955f05e99eedfd0085e4853108934580c911599b529` |
   | layer | 12.16 GiB gz（raw 约 21.6 GiB，与 v0.1 同量级——只换了约 57 KB 的 Python） |
   | 构建 | 373 s（18 GiB raw 时）；推送 306 s @ 41 MB/s |

**教训（写进纪律）**：镜像里的 bundle 是**快照**，`roles.py` 改一次就要重打一次镜像；
否则「代码改了、测试过了、镜像没换」，缺口的关闭是假的。A2 会继续改 `roles.py`，
所以 A2 的验收必须以「镜像 tag 已更新」为前置。

## 5. 部署动作（本轮实际做了的事）

1. 交叉编译带 A1 的静态二进制（13,152,416 字节，`CGO_ENABLED=0`）；确认 `hostNetwork` /
   `hostIPC` / `ClusterFirstWithHostNet` / `host_network` 四个字符串都进了 ELF。
2. 重建控制面镜像 **`library/tai-talea:v0.2`**，manifest
   `sha256:f2cf9a9242afa2d5841e84ba030260d7d538c170e95e98f11783ddf3ef03446d`，layer 5.29 MiB gz。
   这次**把镜像里 baked 的默认配置换成仓库模板（占位 token）**：v0.1 把部署配置直接烤了进去，
   而该 registry 允许匿名 pull —— 等于把 admin token、bootstrap token 和 partner secret 公开了。
3. 生成部署配置 = 模板 + 从旧配置读出的真实 token（脚本读现文件，不臆造凭据），
   经特权 Pod 写到 h100-04 的 `/data/tai-talea/config/tai-talea.yaml`，旧文件留
   `.bak-before-a1`。sha256 核对：本地旧配置 = 节点备份 = `0431f8f9…`，没有隐性漂移。
4. 滚动更新控制面 Pod 到 v0.2：`imageID` digest 与推送的 manifest 逐字一致，**1/1 Running**，
   每 30 s `partner snapshot committed`。
5. 用 `provision` API 建 worker → 抓 spec → **立刻删掉**（避免真的把 306 GB 权重加载起来）。

## 6. 顺带确认的集群事实

- 控制面 Pod 重建会换 IP（`10.222.49.109` → `10.222.49.116`）。写死 IP 的探针会拿到
  `HTTP 000`，不是鉴权失败。要稳定地址就得有 Service，而 SA 没有这个权限。
- 第一次 pull 12 GiB 要 ~7 min；blob 进过 content store 之后再拉是秒级（本次 worker Pod
  创建后十来秒就直接 Running）。
- 这条链路上 `kubectl exec`、`get nodes`、`Service`、`ConfigMap` 全部不可用，能用的只有
  「建 Pod + 读日志 + 读 Pod JSON」。整个排查都是在这三个动作里完成的。

## 7. 第二层：worker 走到 sglang 启动后仍然失败，原因是 GPU 已被占

B1 的修复生效之后又跑了一次 provision，失败点从「参数被拒」推进到了「sglang 起不来」：

```
{"msg":"alert","alert":"service_start_failed","instance_id":"glm53-b1-probe",
 "detail_attempt":"1","detail_error":"sglang process failed during startup: sglang exited unexpectedly with code -9",
 "detail_exhausted":"false","detail_max":"3","detail_role":"decode"}
```

参数校验已经过了（否则还是 422），所以 B1 是成立的。剩下的 `-9` 要单独查。

### 7.1 先排除「这个 Pod 形态看不见卡」

用一个和 worker 完全同形态的 Pod（同镜像、hostNetwork+hostIPC+privileged、`nvidia.com/gpu: 8`）实测：

```
uid=0(root) gid=0(root) groups=0(root),44(video),109
nvidia-smi -L → GPU 0..7 全是 NVIDIA H100 80GB HBM3
torch 2.13.0+cu130 cuda 13.0 / device_count 8 / name0 NVIDIA H100 80GB HBM3
/dev/infiniband 里有 rdma_cm 与 issm*
```

形态没问题：卡能看见、IB 设备在、torch 认到 8 张。

### 7.2 真正的原因：整机 8 张卡已被别人占满

同一个探针里 `nvidia-smi --query-gpu=index,memory.used --format=csv`：

```
0, 63540 MiB    4, 63586 MiB
1, 63586 MiB    5, 63586 MiB
2, 63586 MiB    6, 63586 MiB
3, 63586 MiB    7, 63346 MiB
```

每张卡 81,559 MiB 里已经用掉 **63.5 GiB**，只剩约 18 GiB。而 `--mem-fraction-static=0.60`
要的是每卡约 48 GiB —— **sglang 根本没有内存可分配**。

`nvidia-smi --query-compute-apps` 这时是**空的**。原因是占用者在另一个 PID 命名空间里，
默认看不到。给探针加 `hostPID: true` 之后立刻现形：

```
295039, sglang::scheduler_TP0_EP0, 63530 MiB
295252, sglang::scheduler_TP1_EP1, 63576 MiB
... 一直到 TP7_EP7
```

即**一个 8 卡的 sglang TP8/EP8 服务在整机常驻**。

### 7.3 它是谁

`/proc/<pid>/cgroup` 给出 `0::/system.slice/docker-38c97da8a869….scope` ——
**不在 Kubernetes 里，是一个 Docker 容器**（k8s 的进程在 `/kubepods/...` 下）。
从节点磁盘上的容器元数据读出它的身份：

| 项 | 值 |
| --- | --- |
| 容器名 | `sgl_0521` |
| 镜像 | `vllmascend-789c756e.ecis.zs-shanghai-1.cmecloud.cn/dev/sglang:v0.5.21-cu130` |
| 构建 | 上游 sgl-project GitHub Actions run（`SGLANG_BUILD_COMMIT=e00930c5…`），**不是我们的内部 fork** |
| Cmd | `["/bin/bash"]`（手工起的） |

> 本次只做了**只读识别**：没有 stop、没有 kill、没有改任何东西。
> 这台机器上跑的不是我们的负载，怎么处理需要人来定。

**这是一道硬阻塞**：只要它占着 8 张卡，控制面无论建多少 worker 都起不来，
而且失败信息会一直表现为「sglang exited with code -9」，看上去像代码问题。
识别手法值得记下来：**显存被占但 `compute-apps` 列空 = 占用者在别的 PID 命名空间，
加 `hostPID: true` 再看一次**。

### 7.4 同日晚复查：阻塞未解除

交付收尾后重跑了一次只读快照（探针 `talea-gpu-state-probe`：sglang runtime 镜像 +
`hostPath /` → `/host`，`imagePullPolicy: Never`，用完即删）：

```
0..7, 63540/63586 MiB used of 81559 MiB      ← 与首次识别逐卡一致
/sgl_0521 | vllmascend-…/dev/sglang:v0.5.21-cu130 | running: True
```

宿主上另有 `running: True` 的 `/eval-chy`、`/glm-chy`（vllm 系）——同属第三方负载，
同样只读识别、未动。**结论不变：`sgl_0521` 一天后仍占满 8 卡，A2 的前置没有解除。**

## 8. 遗留与下一步

1. **清掉 h100-04 上那个非 k8s 的 `sgl_0521` 容器**（或换到另一台机器），
   否则 worker 永远停在 `-9`。这是 A2/C1/D1 的前置，比任何代码改动都优先。
2. **A2**（按节点/按角色参数）：`--disaggregation-ib-device` 两侧不同（h100-01 必须剔除 mlx5_6）、
   decode 独有 `--max-running-requests 128` 与 `--cuda-graph-max-bs-decode 128`、
   prefill 独有 `--chunked-prefill-size 512`，加上 NCCL_IB_HCA / NCCL_IB_TC 的节点级 env。
   现在共享列表已经把非角色相关的旗标补齐，A2 只需要引入覆盖机制。
3. **C1**（动态注册 readiness 实测）仍然没做：今晚的 Router 是静态 `--prefill/--decode` 起的。
4. **h100-01** 仍缺 ecis 的 `/etc/hosts` 映射；`node_selector` 因此还钉在 h100-04 单节点。
5. `imagePullPolicy: Always` + tag 会被重建这一事实，意味着**旧 tag 的内容会变**。
   所以每轮换 bundle 都推新 tag（v0.1 → v0.2），不要把新镜像盖到旧 tag 上。
6. 控制面仍是单副本、绑 h100-04；kubeconfig 仍是运维凭据放在 Pod 里。
7. 控制面会为已删除的实例反复重试 `/bootstrap/stop`（`connection refused`），
   直到 `release_max_attempts` 用尽 —— 日志里那几条 ERROR 是正常的收尾，不是故障。
