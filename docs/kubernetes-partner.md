# Kubernetes 命名空间作为容量来源

> 对应实现：`internal/partners/kubernetes`、`partner.SelfProvisioningAdapter`。
> 目标平台：中国移动 AI 平台（`ai.cmss.chinamobile.com`）的 `nightshift` 租户。
> 日期：2026-10-08。

## 1. 为什么需要一条新的接入路径

现有铺装链路（alayanew CCI）是**三段式**：

```
创建容器 → SSH 铺装（推 bootstrap、建 venv、装依赖）→ 纳管
```

这条路依赖一个前提：**平台给你一个能登录的容器**。`nightshift` 租户没有这个前提。实测权限边界：

| 能力 | 结果 |
| --- | --- |
| 建 / 删 / 查 Pod、Deployment、Job | ✅ |
| 读 Pod 日志（`pods/log`） | ✅ |
| `pods/exec` | ❌ |
| `pods/portforward` | ❌ |
| Service / Ingress / ConfigMap / Secret / PVC | ❌ |
| 平台 CRD（`InferenceService`、`PDInference`、`ModelImage` …） | ❌ |
| 容器出网 | ❌（`pypi.org`、内网 registry 全返回 000） |

禁 exec、禁 port-forward、无出网 → **SSH 铺装不可能，容器内也装不了任何东西**。

结论：worker 必须**自带 bootstrap**，创建完成即等于准备完成。这正是 `deploy/container/entrypoint.sh` 已经实现的形态——它本来就是为"受控镜像"路线写的，只是此前没有适配器去用它。

## 2. 语义映射

| 控制面概念 | alayanew CCI | Kubernetes 命名空间 |
| --- | --- | --- |
| 容量单元 | 一个 CCI 容器 | 一个 Pod |
| 容量发现 | `GET /v1/subscription/CCI/list` 过滤 RUNNING | `GET /api/v1/namespaces/{ns}/pods?labelSelector=…` 过滤 Running |
| 身份 | 运维指定的实例名 | Pod 名（写进 `tai-talea.instance` 标签） |
| 租约 | `cci-<name>` | `k8s-<pod UID>` |
| 控制端点 | 平台端口映射的公网 IP | `http://<podIP>:9001` |
| 服务端点 | 平台端口映射 | `http://<podIP>:9002` |
| 归还 | 释放订阅 | 删除 Pod |
| 铺装 | SSH + `provision/ssh_install.py` | **无**：镜像 entrypoint 自带 |

租约用 **Pod UID** 而不是名字，是为了让"同名 Pod 被重建"能被识别成**不同的容器**——否则控制面会把新容器当成老容器继续服务。

## 3. 代码落点

### 3.1 抽象层扩展（`internal/partner/adapter.go`）

`ProvisioningAdapter` 的第三步 `AwaitSSHAccess` 对 k8s 没有意义，于是**并列**引入一个可选接口，而不是修改原接口：

```go
type ProvisionedNode struct {
    ID, Endpoint, ServiceEndpoint, LeaseID string
    Spec domain.InstanceSpec
}

type SelfProvisioningAdapter interface {
    ProvisionNode(ctx context.Context, name string) (ProvisionedNode, error)
}
```

与 `ProvisioningAdapter` 一样用**类型断言**发现，`PartnerAdapter` 这个只读为主的契约一个字都没动。两种形态可以共存：一个部署里 alayanew 走 SSH、k8s 走自带 bootstrap，互不影响。

### 3.2 适配器（`internal/partners/kubernetes/`）

| 文件 | 职责 |
| --- | --- |
| `kubeconfig.go` | kubeconfig / in-cluster 两种凭据解析，含 client certificate、CA、相对路径 |
| `client.go` | 裸 HTTP 的 k8s REST 客户端 + wire 类型 |
| `manifest.go` | Pod 规格构建 |
| `adapter.go` | 三个只读契约方法 + `ProvisionNode` |

**不引入 client-go。** 项目的依赖只有 `yaml.v3` 与纯 Go 的 SQLite，一个单静态二进制。适配器只用四个 Pod 动词，不值得把整个 client 拉进来。kubeconfig 用 `yaml.v3` 解析，TLS 用 `crypto/tls`。

### 3.3 开通入口（`internal/api/provision.go`）

`POST /v1/capacity/console/provision` 现在按适配器能力分流：

```
CreateInstance → WaitCreation → AwaitSSHAccess → onboarding.Submit   （SSH 形态）
ProvisionNode                                 → EnrollPrepared        （自铺装形态）
```

第二条路径**完全不经过 `onboarding.Manager`**，因此 `onboarding:` 配置段可以整段省略，控制面照常工作。

## 4. 1P1D 是怎么发生的

控制面不做 PD 拆分，它只是把角色发给容器：

```
1. POST /v1/capacity/console/provision {partner_id, name: glm53-a, count: 2}
   └─ 每台：创建 Pod（8 卡、挂权重、512Gi /dev/shm、注入 bootstrap token）
      → 等 Running → 拿到 Pod IP
      → EnrollPrepared：实例进入 ALLOCATING → PREPARING

2. 控制器 prepare：GET /bootstrap/health 确认 bootstrap 就绪

3. 控制器 start：POST /bootstrap/start {role: prefill | decode, …}
   └─ 角色由 planner（gear 1:1）分配，extra_sglang_args 里带上
      --disaggregation-transfer-backend / --disaggregation-bootstrap-port
   └─ Pod 内 bootstrap 组装 sglang 命令并拉起

4. 控制器注册：POST /workers {url: http://<podIP>:9002, worker_type: prefill|decode,
                              bootstrap_port: 8998}
   └─ Router 形成 prefill / decode 两个池

5. 摘流仍只走 readiness —— RouterAdapter 接口里没有 DELETE /workers，这一点没变。
```

Pod 创建时**不写死角色**（`TALEA_ROLE` 留空）。角色是控制面在运行时给的，写死会让 planner 失去再平衡能力。

## 5. 部署

三份清单在 `deploy/kubernetes/`：

| 文件 | 内容 |
| --- | --- |
| `tai-talea.yaml` | 控制面配置（含 `partners[].kubernetes` 段） |
| `tai-talea-control-plane.yaml` | 控制面 Pod |
| `talea-router.yaml` | Router Pod（hostNetwork） |

三处**必须**这样安排的原因：

- **控制面在集群内**：worker Pod 地址只有集群内可达。放到集群外，能建 Pod 但驱动不了 bootstrap。
- **Router 用 `hostNetwork`**：没有 Service 权限就固定不了地址；hostNetwork 让 Router 直接绑节点端口，地址在 Pod 重启后不变。
- **权重走 hostPath**：没有 PVC 权限，而模型已经在节点上（实测 `/apps/models/GLM-5.3-Flash`，306 GB，62 个 shard，`chat_template.jinja` 齐备）。

## 6. 踩过的坑（都写进了清单和注释）

| 坑 | 现象 | 处置 |
| --- | --- | --- |
| 设了 `runAsGroup` | `nvidia-smi: command not found`，GPU 全不可见 | 只设 `runAsUser`，保留镜像默认的 `44(video)` 组 |
| `/dev/shm` 默认 64 MB | TP8 多进程通信直接崩 | `emptyDir{medium: Memory, sizeLimit: 512Gi}` |
| GPU 写进 `requests` | 被静默忽略，调度到无卡节点 | 只写 `limits` |
| 权重目录缺失 | warmup 失败、`auto` 解析器识别失败 | 显式 `--chat-template=<model>/chat_template.jinja` |

## 7. 当前阻塞项

1. **镜像**：集群无公网，节点上只有 `nvidia/cuda:13.1.1-cudnn-devel-ubuntu24.04-python3` 一个通用镜像。worker 镜像必须先构建再由平台导入 containerd，或者拿到一个可推送的内网仓库。
2. **控制面的 kubeconfig 或 SA 授权**：清单走的是挂载运维 kubeconfig 的路线，绕过 RBAC 问题。若平台愿意把同样的 Pod 权限授予控制面的 ServiceAccount，则更干净。
3. **`h100-01` 与 `h100-04` 之外的节点**：`h100-02`/`h100-03` 未授权。四台 8 卡的方案受此限制。
