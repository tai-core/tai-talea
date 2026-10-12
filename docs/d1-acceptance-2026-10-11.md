# D1 真机验收：一次请求，跨两台机器的 1P1D

2026-10-11（集群日志为 UTC，本地时间 = UTC+8；本文引用的时间戳一律是集群日志的 UTC）

## 1. 结论

| 验收点 | 结果 | 证据 |
| --- | --- | --- |
| 一条请求即得到两台 worker | ✅ | §5.1（`202` 受理两个 job） |
| 两个 worker 落在**不同**节点 | ✅ | §5.3（h100-04 / h100-01，`tai-talea.node` 标注各自正确） |
| planner 按 `gear=1:1` 分出 **1 prefill + 1 decode** | ✅ | §5.2（`planner round applied target=1/1`） |
| 两台机器的 argv **按角色差异化** | ✅ | §5.2（`--disaggregation-mode` 与 `--disaggregation-ib-device` 两套） |
| 两侧 service 都到 `SERVING` | ✅ | §5.1（`service serving` × 2，各带 `router_worker_id`） |
| Router **双池**都建起来 | ✅ | §5.4（`prefill_count:1, decode_count:1`） |
| Router 判定可路由（此前是 503） | ✅ | §5.4（`/readiness` 200 `healthy_workers:2`） |
| 经 Router 的**真实推理**成功 | ✅ | §5.5（200，1.04 s / 0.71 s，答案正确） |
| 端到端耗时 | 14 分 20 秒 | §5.1（17:22:29 → 17:36:49） |

D1 到此闭环。链路是：**一次 `POST /provision {count:2}` → 建两个 Pod（分落两节点）→
planner 分角色 → `/bootstrap/start` 下发各自 argv → SGLang 起来 → `POST /workers` 注册 →
`SERVING` → 请求经 Router 返回**。整个过程除那一条请求外没有人工干预。

## 2. D1 卡在哪：**控制面加载的配置里只有一台机器**

这是本轮唯一真正的阻塞，值得完整记下来，因为它的表象与根因离得很远。

**表象**：`count=2` 之后两个 Pod 的 `nodeSelector` 逐字相同（都是 `h100-04`），
第二个 Pod `Pending`，事件是 `1 node(s) didn't have free ports for the requested pod ports`。
两个 Pod 的 `tai-talea.node` 标注**都是 `h100-04`**。

**第一个被排除的假设**是「adapter 看到空的 `Nodes`，回落到了 `node_selector`」。
不成立，判据是 env：

```
partner 级 env:  GLOO_SOCKET_IFNAME, NCCL_SOCKET_IFNAME
nodes[0].env:    NCCL_IB_TC, NCCL_IB_HCA
两个 Pod 的 env 里都有 NCCL_IB_HCA = mlx5_0,...,mlx5_8
```

`NCCL_IB_HCA` 全配置只出现在 `nodes[0].env` 里（`tai-talea.yaml:224-228`）。
它能进 Pod，说明 `nextNode()` **返回了节点对象**，不是 `nil`。
回落路径只会给出 `{kubernetes.io/hostname: h100-04}` 一条，也不会带这些 env。

**第二个被排除的假设**是「adapter 被构造了两次，两个 `nodeCursor` 各自归零」。
不成立：`cmd/tai-talea/main.go:117` 的 `partners.Build(cfg)` 在 `serve()` 里只调用一次。

**真因**在宿主上的那份配置本身：

| | 集群上真正加载的 | 仓库里的 |
| --- | --- | --- |
| 路径 | `/data/tai-talea/config/tai-talea.yaml` | `deploy/kubernetes/tai-talea.yaml` |
| 大小 / md5 | 12581 B / `f2cb13228c0700c896dfc8cb0d85bcdb` | 12815 B / `6fc032da3306fde17cafb55b8cf3be8a` |
| mtime | 2026-10-10 01:31:20 +0800 | — |
| `nodes:` 条目数 | **1**（只有 h100-04） | 2（h100-04、h100-01） |

两份文件的 `diff` 只有四处：三处是仓库副本用占位符替代了真实凭据
（`admin_token` / `bootstrap_token` / `push_token`+`push_secret`），**第四处就是
`- host: h100-01` 那一条**。

于是 `nextNode()` 的 `index % len(Nodes)` 恒为 `index % 1 == 0`：轮转逻辑没坏，
它只是被喂了一个长度为 1 的列表。**h100-01 那一条只加在了本地仓库里，从未推上节点。**

> 顺带一个容易误判的细节：`nodeSelector` 里那条
> `node-restriction.kubernetes.io/tenant: nightshift` 是**集群准入策略自己补的**
> （附录 A 早先已记录），不是我们写的。所以「两个 Pod 的 nodeSelector 一样」
> 这件事要看的是 `kubernetes.io/hostname` 那一条，不能整块比较。

**修法**：`config.Load` 只在启动时调用一次（`main.go:63`，没有 watcher），所以改完配置
必须重建控制面 Pod。流程做成三步，避免把一份没校验过的配置推上去：
先在宿主上落成 `tai-talea.yaml.new`（原文件 `cp -a` 备份为 `tai-talea.yaml.bak-20261011T011439`），
再用**控制面镜像自己的** `--check-config` 校验：

```
$ /usr/local/bin/tai-talea --config /etc/tai-talea/tai-talea.yaml.new --check-config
configuration /etc/tai-talea/tai-talea.yaml.new is valid (1 partners, router http://10.214.81.70:8000)
```

校验通过之后才 `mv` 就位（同目录内 `mv` 是原子的），最后重建 Pod。
`--check-config` 这一步是零成本的保险：解码器是同一份字节，它说能读就一定能读。

## 3. 顺带修掉的三处清单漂移

排查过程中发现「集群里跑的东西」和「仓库里写的东西」有四处不一致，除上面那条 `nodes`，
其余三处一并修正。它们都属于同一类问题：**留着一份明天按它重建就会坏的清单。**

| 项 | 集群里实际 | 仓库里原先写的 | 按仓库重建的后果 |
| --- | --- | --- | --- |
| 控制面镜像 | `library/tai-talea:v0.3` | `v0.2` | 退回不含 A2 的旧二进制，严格的解码器会拒掉配置里的 `extra_sglang_args_by_role` |
| kubeconfig 的 hostPath | `/data/tai-talea/config/kubeconfig` | `/root/.kube/config` | 该文件在节点上不存在，控制面取不到 API 凭据 |
| kubeconfig 的 `subPath` | 无 | `subPath: config` | `CreateContainerConfigError: failed to prepare subPath for volumeMount "kubeconfig"`（实测踩到） |
| Router 清单整份 | `talea-router:v0.2` / 端口 8000 / `--pd-disaggregation` | `registry.paas/cmss/sglang-router:0.5.20` / 端口 30000 / 无 PD 参数 | 与生产配置里 `router.url: http://10.214.81.70:8000` 对不上，且没有 PD 池 |

另外 `tai-talea.yaml` 里那段注释「h100-01 迟早要进 `nodes`，现在它还没有 ecis 的
`/etc/hosts` 条目，落到那里会 ImagePullBackOff」已经**不成立**（条目在，
且本次拉取实测成功），改为记录现实。

## 4. Router 清单重写（M3+C2 完成）

`deploy/kubernetes/talea-router.yaml` 从「一份写着 `CHANGE` 的模板」改成了线上运行的那一份，
按实测 spec 逐项对齐：

- 镜像 `library/talea-router:v0.2`（Rust `sgl-model-gateway`），`imagePullPolicy: Always`
- **`args` 是整条命令行**：镜像的 `Entrypoint` 就是那个二进制，所以清单里不写 `command`
- `--host=0.0.0.0 --port=8000 --pd-disaggregation --prefill-policy=session_aware --decode-policy=round_robin`
- `hostNetwork: true` + `dnsPolicy: ClusterFirstWithHostNet`（两者必须同时，否则 kubelet 拒 spec）、
  `nodeSelector: h100-04`、`containerPort/hostPort 8000`
- readiness 探 `/liveness`（`initialDelaySeconds 3 / periodSeconds 5 / failureThreshold 12`）
- **不挂任何卷**，并在注释里写明为什么：worker 池只存在内存里。

## 5. 端到端证据链

### 5.1 时间线（一条请求的全程）

```
17:22:29  POST /v1/capacity/console/provision {"partner_id":"cmcc-k8s","count":2}  → 202
          接受两个 job：talea-cmcc-k8s-1010-172229 / …-1
17:22:32  两个 Pod 几乎同时提交（kubelet SyncLoop ADD）
17:23:05  instance prepared  talea-…-172229      endpoint=http://10.214.81.70:9001
17:23:15  dispatching start   role=decode        落在 h100-04
17:29:57  h100-01 的 worker 镜像拉完（13.06 GB / 7m24s）     ← §6
17:30:13  h100-01 的 bootstrap 起来（0.0.0.0:9001）
17:30:21  service serving      talea-…-172229      role=decode  router_worker_id=9df89fbb-…
17:30:23  dispatching start   role=prefill       落在 h100-01
17:36:45  service serving      talea-…-172229-1    role=prefill router_worker_id=f3121067-…
17:36:49  两侧都 SERVING（driving 循环第 43 轮）
```

总耗时 **14m20s**。拆开看：decode 从下发到 SERVING 7m06s、prefill 6m17s，两个都是模型加载，
不是控制面在等什么它管得了的东西。

### 5.2 控制面侧

两条 `dispatching start`，argv 按角色与机器分别拼出来
（`--disaggregation-ib-device` 的后缀分别是两台机器各自的 HCA 名单）：

```
role=decode   endpoint=http://10.214.81.70:9001
  … --disaggregation-mode 由 bootstrap 拼在内部（见 5.6） …
  --max-running-requests=128 --cuda-graph-max-bs-decode=128
  --disaggregation-ib-device=mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_5,mlx5_6,mlx5_7,mlx5_8

role=prefill  endpoint=http://10.214.81.67:9001
  --disaggregation-ib-device=mlx5_2,mlx5_3,mlx5_4,mlx5_5,mlx5_7,mlx5_8,mlx5_9,mlx5_10
```

planner 的两轮：

```
17:30:21  planner round applied  summary="gear=1:1 target=0/1 serving=0 idle=1 actions=1"
17:36:45  planner round applied  summary="gear=1:1 target=1/1 serving=1 idle=1 actions=1"
```

第一轮 `target=0/1` 是刻意行为：`TargetPrefill = floor(total*ratio)`，`total=1` 时得 0，
`max(1, …)` 只在 `total>=2` 时才介入 —— 所以**单实例永远是 decode**。

### 5.3 落点

```
talea-cmcc-k8s-1010-172229    tai-talea.node=h100-04  node=h100-04  ip=10.214.81.70
talea-cmcc-k8s-1010-172229-1  tai-talea.node=h100-01  node=h100-01  ip=10.214.81.67
```

`tai-talea.node` 是 `manifest.go` 在**建 Pod 之前**就把选中机器写进标签的，所以它记的是
策略的选择本身，与 kubelet 事后调度到哪儿无关 —— 这两个注解是「轮转生效」最干净的证据。

### 5.4 Router 侧双池

```
GET /workers      → total 2
  9df89fbb-…  url http://10.214.81.70:9002  worker_type "decode"  is_healthy true
              metadata.load_balance_method "round_robin"        disaggregation_mode "decode"
  f3121067-…  url http://10.214.81.67:9002  worker_type "prefill" is_healthy true
              metadata.load_balance_method "follow_bootstrap_room" disaggregation_mode "prefill"
  stats {prefill_count:1, decode_count:1, regular_count:0, pending_count:0}

GET /readiness    → 200 {"status":"ready","healthy_workers":2,"available_workers":2,"total_workers":2}
GET /state/workers→ 两条，state 全 "ready"，generation 3 / 5
```

三点值得指出：

1. **`/readiness` 由 503 变 200。** 只有 decode 时 Router 会说
   `insufficient healthy workers` —— 这正是判断「PD 配平了没有」的现成探针，
   比数 `/workers` 的条数更权威。
2. **两个 worker 的 `load_balance_method` 不一样**：prefill 是 `follow_bootstrap_room`、
   decode 是 `round_robin`。这不是我们注册时填的，是 smg 从各自的 SGLang
   `/get_server_info` 反读出来的 —— 说明 Router 真的按 PD 语义在调度，不是只在表里挂了两行。
3. **`/state/workers` 的回答里没有角色**。它是 `readiness-v1` 那层的薄视图
   （`worker_id` / `worker_url` / `state` / `generation`），角色只在 `/workers` 里。
   排查时先看 `/state/workers` 判「能不能被路由」，再看 `/workers` 判「是什么角色」。

### 5.5 真实推理

经 Router 发三次请求，全部 200：

```
"Name the capital of France in one word."   http=200  total=1.035s    content="Paris"
同样的问题第二次                             http=200  total=0.708s    content="Paris"
"What is 17 times 3?"                       http=200  finish_reason="stop"  content="51"
```

`finish_reason: "stop"`（不是 `length`）说明模型自然收尾，`usage` 里
`reasoning_tokens` 与 `completion_tokens` 分开计 —— 与这份权重自身的推理模板一致。

### 5.6 worker 侧

集群禁 exec，所以用 `/bootstrap/status`（带 `X-Bootstrap-Token`）读容器内状态：

```
h100-04 decode : {"phase":"RUNNING","role":"decode","service_pid":964,"exit_code":0,"last_error":""}
h100-01 prefill: {"phase":"RUNNING","role":"prefill","service_pid":963,"exit_code":0,"last_error":""}
```

两者的 `environment` 都是 `ubuntu-24.04 / cuda 13.0 / python 3.12 / sglang 0.5.20` ——
与三处基线身份一致，没有触发 exit 29。

`history[].detail` 是从 `STOPPED → INSTALLING → STARTING` 的完整迁移记录，
`STARTING` 那条给出了 bootstrap 真正执行的命令行（`--disaggregation-mode` 是 bootstrap
按角色拼进 `launch_server` 的，不在控制面下发的 `extra_args` 里）。

## 6. 成本：h100-01 首次拉 worker 镜像 7m24s

containerd 自己的日志给出的数字最干净：

```
01:29:57 (local)  Pulled image "…/library/talea-worker:v0.2"
                  image id "sha256:fcd9acbbd3d4ab10…"
                  size "13058724813" in 7m24.012946265s
```

13.06 GB ÷ 444 s ≈ **29 MB/s**。两条判读规矩：

1. **Pod 卡在 `ContainerCreating` 且没有任何事件，不等于卡死。**
   本集群的 containerd 根不在 `/var/lib/containerd`，而是
   `root = "/apps/data/containerd"`（`/etc/containerd/config.toml`）。
   去错地方数目录，会量出「0 增长」，从而把一次正常的慢拉取误判成故障。
   正确的判据是 `journalctl -u containerd` 里那条
   `stop pulling image …: bytes read=…`，以及最终的 `Pulled image … in <duration>`。
2. 两个节点都**没有**预热这份镜像，所以「第二个 worker 总是比第一个慢」在第一次投放时
   是必然的，与节点无关。预热（或按节点缓存）能把这 7 分钟从关键路径上拿掉。

## 7. 发现的结构性隐患：角色与机器目前是**巧合**对齐的（未修）

这一条不影响本轮验收，但它是一个真实的、会在下一次投放里咬人的设计耦合，写在这里备查。

`--disaggregation-ib-device` 是**机器属性**（每台机器的 HCA 命名不同），
但它现在写在 `controller.extra_sglang_args_by_role` 里 —— **按角色**索引：

```yaml
extra_sglang_args_by_role:
  prefill:
    - --disaggregation-ib-device=mlx5_2,mlx5_3,mlx5_4,mlx5_5,mlx5_7,mlx5_8,mlx5_9,mlx5_10   # 这是 h100-01 的网卡
  decode:
    - --disaggregation-ib-device=mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_5,mlx5_6,mlx5_7,mlx5_8   # 这是 h100-04 的网卡
```

角色由 planner 定、机器由 adapter 的轮转定，**两条决策各走各的**：

- **机器**：`nextNode()` 按 Pod 创建顺序，第 k 个 Pod → `nodes[k % 2]`。名字里没有后缀的那个
  （`…-172229`）先被创建，于是拿 `nodes[0] = h100-04`。
- **角色**：`Plan` 把 idle 实例**按 ID 字典序**排好（`planner.go:305`），
  Phase 1 **先补 prefill**（`planner.go:313`），把 `idle[0]` 给 prefill。
  而没有后缀的名字字典序更小 ⇒ **两个实例同时 idle 时，prefill 会落到 h100-04**。

也就是说：本次之所以正确，是因为两个 Pod **没有同时 idle**——`…-172229` 在 17:30:21
就被判成 decode 了，等到 `…-1` 在 17:36:45 成为 idle 时，planner 只剩它一个可选，
`prefillPlanned(0) < TargetPrefill(1)` 才把它排成 prefill。两轮日志里 `idle=1` 都只有 1，
是这件事的直接证据。**如果 h100-01 已经预热了镜像、两个 Pod 几乎同时 ready，
那么 prefill 会落在 h100-04，被喂到 h100-01 的 HCA 名单**（`mlx5_4 / mlx5_9 / mlx5_10`
在 h100-04 上不存在），表现会是启动期或首次 KV 传输失败。

同一个耦合还有第二半：无后缀 / `-1` 与 `nodes[0] / nodes[1]` 的对应，依赖
`provision.go` 里那两条 goroutine 的启动顺序（`for i … { go s.runSelfProvision(...) }`）。
goroutine 是并发的，这个对应**只是通常成立**，不是被保证的。

**建议的修法**（未实施）：把网卡绑定从角色轴搬到机器轴 ——
在 `nodes[]` 上加一个 `extra_sglang_args`，让 `manifest.go` 在建 Pod 时按**已选中的机器**
合并，与它已经在做的 `nodes[].env` 合并同路。这样「角色」只决定
`--disaggregation-mode` 与 decode 侧那几个 `--max-running-requests`，
不再决定设备名。另一条更省事但更脆的路是给节点加恒定角色
（h100-01 只做 prefill、h100-04 只做 decode），代价是失去「一台机器两种角色都行」的灵活性。

## 8. 复现清单（脚本都在 `.workbuddy/tmp/alayanew/`）

| 用途 | 文件 |
| --- | --- |
| 读出宿主上真正加载的配置 | `d1-cfg2.yaml`（条目数 + 片段）、`d1-cfg3.yaml`（整份 base64 导出） |
| 生成改动后的配置并推上宿主 | `make-cfgwrite.py` → `d1-cfgwrite.yaml` |
| 用控制面镜像自校验 | `d1-cfgcheck.yaml` |
| 原子替换 | `d1-cfgswap.yaml` |
| D1 主驱动器（请求 + 轮询 + 真实推理） | `make-d1-run.py` → `d1-run.yaml` |
| 拉取诊断（四处递进：镜像/写入量/containerd 日志/注册表可达） | `d1-pull.yaml` → `d1-pull5.yaml` |
| 读 worker 的 bootstrap 状态 | `d1-status.yaml` |
| 探 Router 的 API 结构 | `d1-router-probe.yaml` |
| 最终双池 + 推理快照 | `d1-final.yaml` |

操作提醒：调 `ssh-run.py` / `ssh-apply.py` 时在 Git Bash 下要
`export MSYS_NO_PATHCONV=1`，否则命令行里的 `/root/...` 会被转成本地 Windows 路径；
本地路径参数要用 `D:/...` 形式，`/d/...` 形式会被同一个转换吃掉。
paramiko 在 `C:/Users/Tkis1/.workbuddy/binaries/python/envs/default/Scripts/python.exe`
（`versions/` 下的两个解释器都没有）。

## 9. 遗留

| 项 | 归属 | 说明 |
| --- | --- | --- |
| 角色↔机器耦合（`--disaggregation-ib-device`） | 本项目 | §7，建议按机器轴绑定 |
| `extra_sglang_args_yaml` 的 goroutine 顺序依赖 | 本项目 | §7 末段；`nextNode()` 与 `for i … go` 之间应显式定序 |
| 宿主配置与仓库配置无一致性检查 | 本项目/运维 | 本轮根因就是二者不一致；`--check-config` 只校验语法，不比对差异 |
| 预热 worker 镜像 | 运维 | §6，13.06 GB；预热可省下首次投放的 7 分钟 |
| `bootstrap._probe_http_health` 未捕 `http.client.HTTPException` | 本项目 | C1 文档 §7 已列，本轮未改 |
| h100-01 的 ecis `/etc/hosts` 条目 | 平台 | **已具备**，本条从 C1 的遗留里销账 |
| 给控制面 SA 授权（现靠挂 kubeconfig） | 平台 | 不变 |
| 给 `talea-router` 一个 Service | 平台 | 不变；现在仍靠 hostNetwork + nodeSelector 固定成 `10.214.81.70:8000` |
