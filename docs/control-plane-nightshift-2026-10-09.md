# tai-talea 控制面落进 nightshift 集群（2026-10-09）

> 关闭缺口清单里的 **M1**（控制面进集群）。这一步是其余全部缺口的前置：
> adapter 改动、bootstrap 白名单、Router 注册语义，都要有控制面在集群里跑才测得到。
>
> 本页只讲 M1。worker 侧镜像是 `talea-worker-selfcontained-image-2026-10-09.md`。

## 1. 结论

控制面已经在 nightshift 里跑起来了，是一个普通 Pod：

```
NAME        READY   STATUS    RESTARTS   AGE    IP              NODE
tai-talea   1/1     Running   0          2m9s   10.222.49.109   h100-04
```

它同时验证四件事，缺一条都跑不到 `1/1`：

| # | 验证点 | 证据 |
| --- | --- | --- |
| 1 | 镜像能被节点 containerd 自动拉取 | `imageID` digest 与推送的 manifest 逐字一致 |
| 2 | 静态二进制在**无 shell** 的容器里正常执行 | 进程起来了并写出 JSON 日志 |
| 3 | 配置解析 + SQLite 在 hostPath 上落盘 | `control plane listening`，state 目录被写 |
| 4 | kubeconfig 权限足够，控制面真能指挥集群 | 每 30s 一次 `partner snapshot committed` |

第 4 条是实质性的：pull 循环打的是真实的 Kubernetes API。

## 2. 产物

### 控制面镜像

```
shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn/library/tai-talea:v0.1
  manifest  sha256:4ac1f12946d49b713e502c48eb0ffc4cd3a4218324524388fed7e717396080fd
  layer     sha256:abaf657962ea…   5.29 MiB gz  (raw 约 13 MiB)
  diff_id   sha256:10f261653f27…
```

内容是一个**刻意最小的 rootfs**，没有任何 shell：

```
/usr/local/bin/tai-talea        静态 Go 二进制, 0755
/etc/tai-talea/tai-talea.yaml   默认配置（兜底；运行时被 hostPath 覆盖）
/etc/passwd  /etc/group  /etc/nsswitch.conf  /etc/os-release
/tmp (1777)  /var/lib/tai-talea  /proc  /sys  /dev
```

不需要 shell，是因为 Pod spec 用绝对路径覆盖了 entrypoint，探针走 HTTP。
构建脚本 `talea-control-image.py`，纯标准库，无 docker。

### 二进制

```
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w"
→ 13,152,416 bytes, 静态链接（modernc.org/sqlite 是纯 Go，无 cgo）
```

### hostPath 布局（节点 h100-04）

```
/data/tai-talea/config/tai-talea.yaml   0644  ← 目录挂到 /etc/tai-talea
/data/tai-talea/config/kubeconfig       0600  ← 文件挂到 /etc/tai-talea-kubeconfig
/data/tai-talea/state/                  0755  ← 挂到 /var/lib/tai-talea（SQLite）
```

## 3. 配置修正（相对 `deploy/kubernetes/tai-talea.yaml`）

模板是照着理想平台写的，实际跑之前改了八处。每处都有具体理由，不是随便调的：

| # | 项 | 模板值 | 改为 | 理由 |
| --- | --- | --- | --- | --- |
| 1 | `router.url` | `http://10.214.81.67:30000` | `http://10.214.81.70:8000` | 指向实际在跑的 Router（hostNetwork 在 h100-04） |
| 2 | `kubernetes.image` | `registry.paas/cmss/talea-worker:…` | ecis 的 `library/talea-worker:v0.1` | 前者不存在；节点拉不到公网 |
| 3 | `image_pull_policy` | `IfNotPresent` | `Always` | 没有节点预置这个镜像，`IfNotPresent` 会去翻本地然后失败 |
| 4 | `node_selector` | `gpu: h100` | `kubernetes.io/hostname: h100-04` | `gpu` 标签无法验证（token 连 nodes 都读不到），不满足的 selector 会让 Pod 永久 Pending |
| 5 | `profile_path` | `/etc/tai-talea/profile.json` | `/opt/tai-talea/profile.json` | **会出人命的那个**，见下 |
| 6 | `extra_sglang_args` | 含 `--disaggregation-transfer-backend=mooncake_tcp` | 删掉 | 内部 fork 硬编码 mooncake rdma，不接受 tcp |
| 7 | `run_as_user` | `65532` | 删掉 | 镜像里没有这个 uid；验证过的 manifest 都以 root 跑 |
| 8 | kubeconfig 挂载点 | `/etc/tai-talea/kubeconfig`（嵌在 `/etc/tai-talea` 内） | `/etc/tai-talea-kubeconfig`（平级） | 在已挂载目录内部再挂文件，结果依赖挂载顺序 |

### 第 5 条展开

`entrypoint.sh:5` 和 `bootstrap/__main__.py:32` 的默认都是 `/etc/tai-talea/profile.json`，
而 worker 镜像把 profile.json 放在 `/opt/tai-talea/profile.json`（它的 Config.Env 里也这么写）。

adapter 在 `manifest.go:47` 做的是 `env[TAI_TALEA_PROFILE] = cfg.ProfilePath` —— **注入会覆盖镜像自己的 env**。
所以模板那个值会让容器去找一个不存在的文件，自检拿不到 `cuda`，**exit 29、实例被释放**。
症状会是"实例莫名其妙被回收"，非常难倒查。

## 4. 部署步骤（可复现）

```bash
# 1. 交叉编译（本机，Go 1.27.1）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
  -o tai-talea-linux ./cmd/tai-talea

# 2. 构建并推镜像（本机，纯 stdlib，本机能直连 ecis）
python3 talea-control-image.py all

# 3. 投放 config 与 kubeconfig 到节点 hostPath
#    特权 Pod + hostPath / → 写 /host/data/tai-talea/{config,state}
#    config 与 kubeconfig 均以 base64 内嵌在 Pod args 里（SFTP 在本沙箱不可用）

# 4. 起控制面
kubectl apply -f control-plane.yaml
```

## 5. 验证输出

控制面日志（启动即完成全部初始化）：

```json
{"msg":"control plane recovery finished","checked":0,"lost":0,"recovered":0,...}
{"msg":"startup recovery complete",...}
{"msg":"partner pull enabled","partner_id":"cmcc-k8s","interval":"30s"}
{"msg":"capacity control plane listening","address":"0.0.0.0:8787",
 "router":"http://10.214.81.70:8000","gear":"1:1","partners":"cmcc-k8s"}
{"msg":"partner snapshot committed","instances":0,"added":0,"updated":0,"revoked":0}   ← 每 30s
```

`instances=0` 是**正确的**：pull 只认带 partner 标签的 Pod，而命名空间里现有的
Pod 都是手工起的，没有那个标签。

API（从集群内另一个 Pod 打）：

```
TCP 8787                                    OK
GET /                                      200
GET /v1/capacity/console/instances         200  {"count":0,"instances":null}   (Bearer admin_token)
同上带 X-Admin-Token                        401  ← 鉴权确实在生效
```

一条 `pd_ratio_drift` 告警是预期的：planner 在按 1:1 对账，而当前 serving_count=0。
它反过来证明 planner 循环在跑。

## 6. 顺带确认的集群事实

- **RBAC 又窄了一格**：`kubectl get nodes` 现在返回
  `Forbidden: cannot list resource "nodes" at the cluster scope`。
  之前记录的是"两节点只读"，现在连读都没了 —— 所以任何自定义节点标签都**无法验证**，
  node_selector 只能依赖 K8s 自带的 `kubernetes.io/hostname`。
- 节点上有 `/usr/bin/kubectl`（60 MB），节点根盘 196 G 用 50 G。
- `/data` 在节点上存在且 root 可写，模板里选这个路径是对的。
- 从 Pod 网络到 apiserver（`192.168.203.2:6443`）TCP 可达。

## 7. 遗留

1. **控制面是单副本、绑死在 h100-04**。因为 node_selector 钉了主机名，
   而 h100-01 既没有 ecis 的 hosts 映射、也没有缓存镜像。要真正双机，
   得先解决 h100-01 的 hosts（平台统一配，或用既有的 ctr tar 链把镜像搬过去）。
2. **kubeconfig 是运维凭据**，放在 Pod 里等于把运维权限交给任何能进这个 Pod 的人。
   当前 RBAC 禁 exec，风险可控。正解是让平台给控制面 SA 授权
   （那时删掉 kubeconfig 卷，改配 `kubernetes.service_account`）。
3. **控制面还没有被"驱动"过**。它现在只是在空转（pull + planner）。
   真正创建 worker 需要 A1（特权/hostNetwork/hostIPC 开关）、
   A2（按节点与角色的参数）、B1（bootstrap 白名单）三项代码改动。
4. `deploy/kubernetes/tai-talea-control-plane.yaml` 需要按第 3 节的八条更新，
   否则它下次直接 apply 还是起不来。

## 8. 下一步

按缺口文档的依赖顺序，M1 之后最短路径是 **A1 → B1 → A2**：

- **A1** 加 `HostNetwork / HostIPC / Privileged`（0.5 天）—— 不改这个，
  provision 出来的 worker 连 RDMA 设备都摸不到，测不了 A2。
- **B1** 扩 `ALLOWED_EXTRA_FLAGS`（0.5 天）—— 否则 `--disaggregation-ib-device`
  在 bootstrap 层就被拒，A2 的参数根本下发不到 sglang。
- **A2** 角色级与节点级覆盖（1 天）。

做完这三项，才轮到 C1（动态注册 readiness 实测）和 D1/D2（端到端验收）。
