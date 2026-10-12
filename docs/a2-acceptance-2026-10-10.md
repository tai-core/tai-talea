# A2 真机验收：按角色与按节点的参数覆盖

2026-10-10（集群日志为 UTC，本地时间 = UTC+8）

## 1. 结论

| 验收点 | 结果 | 证据 |
| --- | --- | --- |
| 角色级参数合并：prefill 与 decode 收到不同的 `extra_args` | ✅ | §3 的两条日志 |
| prefill 独有设备表，**不含** `mlx5_6`（h100-01 上的坏卡） | ✅ | §3.1 |
| decode 独有 `--max-running-requests=128` / `--cuda-graph-max-bs-decode=128` | ✅ | §3.2 |
| 共享段两侧都在，且**每个旗标只出现一次** | ✅ | §3.3 |
| 节点级 `env`（`NCCL_IB_HCA` / `NCCL_IB_TC`）进入 Pod | ✅ | §4 |
| 落点标签 `tai-talea.node` | ✅ | §4 |
| 主机开关随配置变化（`dnsPolicy` 联动） | ✅ | §5 |
| **SGLang 真正把这些参数跑起来** | ❌ | §7，被外部 GPU 占用阻塞，与 A2 无关 |

**这一轮拿到的是「控制面实际下发给 SGLang 的命令行」，不是「SGLang 成功服务」的证据。**
两者的差别在 §7 写清楚了。

## 2. 验收怎么设计的

### 2.1 观察窗：把命令行写进控制面日志

验收前控制面**不记录**它下发了什么参数：`launcher.Client.Start` 把 202 响应体丢掉了
（`destination = nil`），而渲染出的命令行恰恰就在那个响应体的 `detail` 里
（bootstrap 的 `service.py` 用 `StartResult(True, PHASE_RUNNING, self.state.command)` 返回它）。

所以 v0.3 在 `internal/controller/lifecycle.go` 下发前加了一行：

```go
c.log().Info("dispatching start to the bootstrap",
    "instance_id", instanceID, "role", string(role), "endpoint", instance.Endpoint,
    "model_id", request.ModelID, "extra_args", strings.Join(request.ExtraArgs, " "))
```

放在**调用之前**是关键：日志不依赖 worker 是否真的起来，因此即使 SGLang
随后启动失败，命令行依然留在了控制面日志里。

### 2.2 为什么必须同时有两个实例

`planner.Plan` 的目标数是 `TargetPrefill = floor(total * ratio)`，而
`total >= 2` 时才有 `TargetPrefill = max(1, min(total-1, ·))`（`planner.go:251-256`）。

**只有一个实例时 `TargetPrefill` 恒为 0**，那一台只能被分到 decode —— 实测正是如此（§3 前的
`glm53-a2-prefill` 名字虽带 prefill，planner 给的角色是 decode，见 §6.2）。
所以要看到 prefill 的命令行，必须让 prefill 和 decode 同时各有一个实例。

### 2.3 为什么要临时放开两项设置

同节点两个 worker 在生产配置下不可能同时活着，原因是两条独立的硬约束：

1. **hostNetwork**：endpoint 取的是 `pod.Status.PodIP`（`adapter.go:347`）。开了 hostNetwork，
   同一个节点上所有 worker 的 Pod IP 都等于节点 IP，于是第二个实例 enrol 直接被拒：
   `endpoint already belongs to another active node`（实测日志，见 §6.1）。
2. **`gpu_count: 8`**：一个 worker 声明整节点的 8 张卡，第二个 Pod 永远无法调度。

于是验收用了一份**只改这两行**的配置（§6.3）。被测对象（角色参数、节点 env、三层 env 合并）
一个字没动。

## 3. 证据：两侧命令行

完整链路：`provision count=2` → planner 分配 prefill/decode → 控制面下发 → 日志。

### 3.1 prefill

```
2026-10-09T17:28:44Z  role=prefill  endpoint=http://10.222.49.73:9001
  --disaggregation-bootstrap-port=8998 --tp-size=8 --ep-size=8 --kv-cache-dtype=bfloat16
  --dsa-prefill-backend=tilelang --dsa-decode-backend=tilelang --moe-runner-backend=deep_gemm
  --disable-shared-experts-fusion --attention-backend=dsa --reasoning-parser=glm45
  --tool-call-parser=glm47 --served-model-name=glm-5.3-flash --disable-custom-all-reduce
  --disable-flashinfer-autotune --mem-fraction-static=0.85 --load-format=runai_streamer
  --chunked-prefill-size=512 --enable-metrics --enable-cache-report
  --chat-template=/models/GLM-5.3-Flash/chat_template.jinja
  --disaggregation-ib-device=mlx5_2,mlx5_3,mlx5_4,mlx5_5,mlx5_7,mlx5_8,mlx5_9,mlx5_10
```

### 3.2 decode

```
2026-10-09T17:30:03Z  role=decode  endpoint=http://10.222.49.82:9001
  ... （共享段与 prefill 逐字相同） ...
  --max-running-requests=128 --cuda-graph-max-bs-decode=128
  --disaggregation-ib-device=mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_5,mlx5_6,mlx5_7,mlx5_8
```

### 3.3 两份命令行的差别（这就是 A2 的全部主张）

| 旗标 | prefill | decode |
| --- | --- | --- |
| `--max-running-requests` | 无 | `128` |
| `--cuda-graph-max-bs-decode` | 无 | `128` |
| `--disaggregation-ib-device` | `mlx5_2,3,4,5,7,8,9,10`（**无 `mlx5_6`**） | `mlx5_0,1,2,3,5,6,7,8`（**有 `mlx5_6`**） |
| 其余 20 个旗标 | 完全相同 | 完全相同 |

三点值得点名：

* **合并是按旗标替换，不是追加**：两侧的旗标集合是「共享段 ∪ 角色段」，
  共享段里的旗标没有出现第二次。这是 `mergeSGLangArgs` 的承诺，实测成立
  （否则命令行里会出现两个 `--disaggregation-ib-device`，只靠顺序决胜）。
* **`--chunked-prefill-size=512` 出现在两侧**，因为它在共享段里。它看起来像
  「prefill 的东西」，其实两边都要 chunk；真正分角色的只有批大小上限。
* **两侧的设备表指向同一台物理机上的不同卡名集合**，而 `mlx5_6` 只在 decode 表里。
  这正是那台坏卡的处理方式：名字绑机器，不绑角色（§6.4）。

## 4. 证据：节点级 env

两个 worker Pod 的容器 env（截取）：

```
GLOO_SOCKET_IFNAME         = bond1.1081     <- partner env
NCCL_SOCKET_IFNAME         = bond1.1081     <- partner env
NCCL_IB_HCA                = mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_5,mlx5_6,mlx5_7,mlx5_8   <- nodes[0].env
NCCL_IB_TC                 = 106            <- nodes[0].env
TAI_TALEA_BOOTSTRAP_PORT   = 9001           <- 注入
TAI_TALEA_BOOTSTRAP_TOKEN  = <43 chars>     <- 注入
TAI_TALEA_PROFILE          = /opt/tai-talea/profile.json   <- 注入
TAI_TALEA_SERVICE_PORT     = 9002           <- 注入
TAI_TALEA_START_WAIT_SECONDS = 120
```

三层优先级 `注入 < partner env < node env` 成立：`NCCL_*` 若写成 partner 级也能被
节点级覆盖，而这里节点级恰好只补了两条 NCCL 变量。

Pod 上的落点标签也在：`tai-talea.node: h100-04`。

## 5. 证据：主机开关的联动

验收配置里 `host_network: false`，Pod 上：

```
hostNetwork = (未设置)   hostIPC = true   privileged = true   dnsPolicy = ClusterFirst
```

`dnsPolicy` 是 `ClusterFirst` 而不是 `ClusterFirstWithHostNet` —— **这是正确行为**：
那个策略只在 hostNetwork 打开时才写，否则 kubelet 会拒绝它。A1 的单测
（`TestBuildPodGrantsHostAccessOnlyWhenConfigured`）断言的正是这条联动。

## 6. 过程中冒出来的三个事实

### 6.1 hostNetwork 把「一节点一个 worker」变成了硬约束

第二条 enrol 失败的原文：

```
ERROR enrolment failed provision=glm53-a2-prefill partner=cmcc-k8s
      instance=glm53-a2-prefill error="endpoint already belongs to another active node"
```

当时那条其实是**旧实例还没 drain 完**造成的。但它揭示的机制是真的：
hostNetwork 下 endpoint = 节点 IP + 9001，同节点任意两个 worker 都撞地址。
所以 `nodes` 列表的轮转不是「锦上添花」，是**唯一**能让两个 worker 分开的手段。

### 6.2 实例名和角色没有关系

`provision` 时给的 `name=glm53-a2-prefill` 只是标识，planner 独立决定角色 ——
那一台实际被分成了 decode。命名成 `-prefill` 不产生任何约束。

### 6.3 单个实例永远是 decode

见 §2.2。想做单角色验收（比如只想看 prefill 的命令行）**在现有 planner 下做不到**：
`gears` 只有 `1:1 / 1:2 / 2:1`，没有 `1:0` 或 `0:1`（实测 `invalid_gear`）。

### 6.4 设备表绑机器，不绑角色

`extra_sglang_args_by_role` 这个名字有误导性。`--disaggregation-ib-device` 的两份取值
是**两台机器各自的好卡清单**，只因为当前「角色 ↔ 机器」一一对应才放在角色段里。
把某个角色挪到另一台机器，两份清单都得跟着改 —— 模板注释与
`role-and-node-argument-overrides-2026-10-10.md` 都写了这一条。

## 7. 没验到的部分

**SGLang 本身没有真正跑起来**，两侧的 `dispatching start` 之后都是启动失败。
原因与本轮的改动无关：

```
GPU 0..7: 63.5 GiB used / 81.6 GiB total，每卡只剩约 18 GiB
while --mem-fraction-static=0.85 needs ≈ 58 GiB/卡
```

占卡者是**非 k8s 的 Docker 容器** `sgl_0521`（`…/dev/sglang:v0.5.21-cu130`，上游 build），
`State.Running: true`，`/proc/<pid>/cgroup` 落在 `system.slice/docker-….scope`。
**本轮全程只做只读识别，没有 stop / kill 任何东西。**

所以下面这条**还没被验证**，它是 A2 之外的下一个真实验收点：

> 参数送到 SGLang 手里之后，PD 两侧能不能用这份设备表把 KV 传起来。

拿到卡之后，这一步的验收入口是控制面日志里那行 `dispatching start`（现在有了），
以及 Router 侧的注册结果。

另外两处已知缺口，与本轮无关但会影响完整 PD：

* **h100-01 仍缺 ecis registry 的 `/etc/hosts` 映射**，所以它上面调度不出 worker
  （`ImagePullBackOff`）。这也是本轮 `nodes` 只列 h100-04 的原因。
* 平台尚未给控制面的 ServiceAccount 授予 Pod 权限，kubeconfig 挂载仍是唯一可行路径。

## 8. 落地动作与现场恢复

| 动作 | 结果 |
| --- | --- |
| 控制面代码 | `lifecycle.go` 增加 start 派发日志；`go build`/`go vet`/`go test ./...` 全绿 |
| 控制面镜像 | `library/tai-talea:v0.3`，manifest `sha256:9090b1c1a193…`，layer `sha256:cd0329d7923a`（5.30 MiB gz） |
| 生产配置 | `host_network: true`、`gpu_count: 8`、`nodes: [h100-04]`，sha256 `7365e7e4dc36…` |
| 验收配置 | 同上，仅 `host_network: false` + `gpu_count: 4`，sha256 `8920b781f932…`（现场已还原为生产值） |
| 控制面 Pod | `tai-talea` 1/1 Running（v0.3），IP `10.222.49.84` |
| 实例 | 5 个（3 个 A1/B1 遗留 + 2 个本轮）全部 reclaim 并释放，worker Pod 已删除 |
| 探针 Pod | 全部删除；命名空间只剩控制面、`talea-worker-service` 与 22 h 前的 PD 残留 |

配置的来回切换都走 hostPath 上的落盘文件，`stage-config-v03.yaml` / `stage-config-v04.yaml`
可在 `.workbuddy/tmp/alayanew/` 复查，切换时留了 `.bak-before-a2`。
