# 节点权限清单（kunhaoyuan，2026-11-01）

为"收紧节点权限"这件事做的盘点。**所有结论都来自线上运行的 spec 或实测**，
推断的部分会标出来。复现命令见 §7。

## 0. 一句话结论

**没有特权容器**（三个工作负载都没有 `privileged: true`），**没有 hostNetwork / hostPID /
hostIPC**，也**没有 DaemonSet**。但三个工作负载**都没有写 `securityContext`**，
所以现在实际上是以 **root（uid 0）+ 默认 capabilities + 无 seccomp 限制** 在跑。

真正需要收紧的面只有两类：

1. **5 条宿主机路径**（4 条 hostPath + 1 条 16 Gi 共享内存），其中 3 条可写；
2. **control 容器里那份 kubeconfig**——它是我们唯一一个持有集群凭据的容器。

GPU 本身不靠特权：`nvidia.com/gpu` 由容器运行时（CDI）注入，pod spec 里看不到任何
device 或 `/dev/nvidia*` 挂载，所以关掉特权模式不影响用卡。

## 1. 三个工作负载的安全属性

| 项 | talea-router | talea-control | talea-worker ×4 |
| --- | --- | --- | --- |
| 镜像 | `glm-5.3-flash-ailog4` | `talea-control-0.9.10` | `talea-worker-0.9.2` |
| 镜像内 `USER` | 未设置 ⇒ **root** | 未设置 ⇒ **root** | 未设置 ⇒ **root** |
| `privileged` | 未设置 ⇒ false | 未设置 ⇒ false | 未设置 ⇒ false |
| `capabilities` | 未设置 ⇒ 运行时默认集 | 同左 | 同左 |
| `allowPrivilegeEscalation` | 未设置 ⇒ **true** | 同左 | 同左 |
| `seccompProfile` | 未设置 ⇒ unconfined | 同左 | 同左 |
| `readOnlyRootFilesystem` | 未设置 ⇒ 可写 | 同左 | 同左（要写 `/opt/tai-talea/run`） |
| `runAsNonRoot` | 未设置 | 未设置 | 未设置 |
| `hostNetwork` / `hostPID` / `hostIPC` | 全 false | 全 false | 全 false |
| ServiceAccount | `default` | `default` | `default` |
| GPU | — | — | `nvidia.com/gpu: 4`（不占特权） |

> `USER` 是从镜像 config 里读的（`/v2/…/blobs/<config>`，§7.3），不是看 Dockerfile。
> 三个镜像的 `User` 字段都是空，即 uid 0。

## 2. hostPath 明细（这是主要风险面）

| 宿主机路径 | 容器内 | 读写 | 谁挂 | 里面是什么 | 为什么必须挂 |
| --- | --- | --- | --- | --- | --- |
| `/data02/model-weight` | `/models` | **ro** | router、worker | 190 GB 权重（含在用的 `GLM-5.3-Flash-NVFP4`，以及 DeepSeek、GLM-5.3 等其它模型） | worker 加载模型；**router 只需要 tokenizer 那几个文件**，却拿到了整棵树 |
| `/data02/talea-ai/logs` | `/logs` | **rw** | router | `talea-router.log`（stdout 镜像） | 容器内唯一 sink 是 stdout，靠 hostPath 落盘供采集 |
| `/data02/talea-glm53/cache` | `/root/.cache` | **rw** | worker | torch/triton/sglang 编译缓存 | 跨重启复用，冷启 5 分钟 vs 11 分钟 |
| `/data02/talea-glm53/logs` | `/logs` | **rw** | worker | sglang/worker 日志 | 同上，落盘 |
| （emptyDir, `medium: Memory`, 16 Gi） | `/dev/shm` | rw | worker | 进程间共享内存 | NCCL / torch 多进程；**计入节点内存**，不是 hostPath |

非 hostPath 的卷（低风险，但列全）：

| 卷 | 类型 | 谁 | 内容 |
| --- | --- | --- | --- |
| `config` | ConfigMap `talea-config` | control | 控制面配置 |
| `kubeconfig` | **Secret `talea-kubeconfig`** | control | 集群 API 凭据（见 §4） |
| `state` | emptyDir（磁盘） | control | 实例表，重启即空 |
| SA token | projected | 三者 | `/var/run/secrets/kubernetes.io/serviceaccount` |
| `harbor-cred` | imagePullSecret | 三者 | registry 拉取凭据（kubelet 使用，不落容器） |

## 3. 系统调用

### 现状

没有任何容器设置 `seccompProfile`。在没有显式打开 kubelet `SeccompDefault` 的集群上，
这意味着容器以 **unconfined** 运行——不是"默认允许集"，是"完全不拦"。

### 已实测（今天做的，不是推断）

用 worker 镜像起了一个 1 卡探针，同时施加：

```yaml
securityContext: { seccompProfile: { type: RuntimeDefault } }   # pod 级
containers[].securityContext:
  allowPrivilegeEscalation: false
  capabilities: { drop: ["ALL"] }
```

跑 `torch.cuda.device_count()` + `torch.zeros(1024, device='cuda')`：

```
devices 1
sum 0.0
TORCH_OK          # exit 0，用时 4 秒
```

**说明驱动打开、显存分配这条路径不依赖被默认 seccomp 拦掉的系统调用**，
`drop: ALL` 也不影响用卡。

### 还没测的（要老实说）

完整"加载 190 GB 权重 + 真跑一次推理"没在受限配置下跑过，那需要 4 卡 + 约 4 分钟。
已知与这几个 syscall 家族相关，建议在受限配置下单独验一次：

| 家族 | 谁需要 | 典型调用 |
| --- | --- | --- |
| 驱动/显存 | worker | `ioctl`（NVIDIA）、`mmap`/`munmap`/`madvise`、`mlock`/`munlock` |
| 线程/调度 | 三者 | `clone`/`clone3`、`futex`、`sched_getaffinity`/`sched_setaffinity`、`prctl`、`membarrier` |
| 网络 | 三者 | `socket`、`bind`、`listen`、`sendmsg`/`recvmsg`、`epoll_*` |
| 共享内存 | worker | `memfd_create`、`shmget`/`shmat`（`/dev/shm`） |
| 文件 | 三者 | `openat`、`statx`、`pread64`/`pwrite64`、`fsync` |

> `RuntimeDefault` 是 Docker/containerd 的默认白名单，上面这些**都在白名单里**，
> 所以大概率能直接上；真要更严就得自己写 profile，做法是先跑一次完整启动并在
> 节点 `dmesg`/audit 里收 `SECCOMP` 拒绝对，逐条补。

## 4. 两个需要平台回答的点

1. **`talea-kubeconfig` 的权限范围**。control 用它访问集群 API；我们只能列出这个
   Secret 的名字（`kubectl get secret` 可见），**读不到内容，也读不到 RoleBinding**
   （`roles`/`rolebindings`/`serviceaccounts` 都被 RBAC 拒）。需要平台给出：
   这个身份能对哪些资源做什么（我们观察到的行为是"能列 Pod、能建/改 Deployment"）。
2. **GPU 是怎么注入的**。pod spec 里没有任何 `/dev/nvidia*` 挂载或 device 条目，
   说明是容器运行时 / CDI 直接注入。收紧权限时要确认 CDI 仍放行 `/dev/nvidia*`，
   否则卡在、模型却起不来。

## 5. 建议的收敛顺序（按代价从低到高）

| # | 改动 | 影响面 | 风险 | 验证方式 |
| --- | --- | --- | --- | --- |
| 1 | 三个工作负载都加 `allowPrivilegeEscalation: false` + `capabilities.drop: ["ALL"]` | 全部 | **低**（GPU 路径已实测不受影响） | 拉起后跑一次推理 |
| 2 | 加 `seccompProfile: RuntimeDefault` | 全部 | 低-中 | 先用一个 4 卡副本跑完整加载 + 一次 `/generate` |
| 3 | router 的 `/models` 从"整棵 190 GB"收窄为几个 tokenizer 文件（`hostPath: {type: File}`） | router | 低 | 起一次看能不能 tokenize |
| 4 | worker 的 cache/logs 从 hostPath 改 PVC 或 emptyDir（helm chart 已经是这个形态） | worker | 中：丢了缓存冷启 11 分钟 | 冷启计时对比 |
| 5 | worker 改非 root（镜像里改目录属主） | worker | 中-高：CUDA/缓存目录权限 | 完整加载 + 推理 |
| 6 | namespace 上 PSA `baseline`（`restricted` 与 hostPath 天然冲突） | 全部 | 高 | 全量回归 |

第 1、2 条不需要改代码，只改 `30-router.yaml` / `40-control.yaml` / `gen-workers.py`，
而且**第 1 条已经实测过不破功能**。

## 6. 明确没有的东西

- 没有 `privileged: true`；
- 没有 `hostNetwork` / `hostPID` / `hostIPC`；
- 没有 DaemonSet；
- 没有给容器加任何额外 `capabilities.add`；
- 没有在 spec 里 `sysctls`；
- 没有 `hostPort`。

## 7. 复现命令

```bash
# 7.1 三个负载的 pod 级安全属性
kubectl get pods -o json | jq '.items[] | {name:.metadata.name,
  hostNetwork:.spec.hostNetwork, sc:.spec.securityContext,
  ctr:[.spec.containers[]|{n:.name, sc:.securityContext}]}'

# 7.2 hostPath
kubectl get deploy talea-w0 talea-router -o json | jq '.items[].spec.template.spec.volumes'

# 7.3 镜像默认用户（需要 registry 凭据）
#    GET /v2/khy/gongji/manifests/<tag> → config.digest → GET /v2/khy/gongji/blobs/<digest>
#    看 .config.User，空 = root

# 7.4 受限配置下验证 CUDA（1 卡，约 30 秒，跑完记得删）
#    见 docs 里 §3 的 pod 片段：seccompProfile RuntimeDefault + drop ALL +
#    allowPrivilegeEscalation false，命令是 torch 初始化 + 一次显存分配
```
