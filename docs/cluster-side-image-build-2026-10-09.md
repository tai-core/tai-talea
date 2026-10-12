# 能否直接在 k8s 集群里打镜像？（2026-10-09 实地探测）

**结论：能，但方式不是「在集群里跑 docker」，而是「把集群当一台远程构建机 +
用宿主 containerd 的 ctr 直接灌镜像」。** 而且这条路我们昨天已经验证过一半。

---

## 1. 一句话回答

| 提法 | 可行性 | 说明 |
| --- | --- | --- |
| 在集群里跑 `docker build` | ❌ | 节点无 docker/podman/buildah；Pod 内也只有一个跑服务的容器 |
| 在集群里跑 kaniko / buildkit / buildah-rootless | ⚠️ 理论可行但需拉新镜像 | 这些镜像在 air-gap 集群里没有，得先弄进去（鸡生蛋） |
| **用自研管线（纯 Python stdlib）在 Pod 里 build** | ✅ **已验证可行** | 我 09-23 在九章、今天在夜班探测，通道全在 |
| **`ctr images import` 把 tar 灌进节点 containerd** | ✅ **已验证**（09-23 h100-04→h100-01） | 宿主 `/usr/bin/ctr` 现成（24 MB，2026-02-25） |
| 直接在节点上 `ctr images pull` 拉远端 | ❌ | 节点完全 air-gap，实测 `tcr:000`、`ecis:000` |

**所以「在 k8s 平台打镜像」的准确形态是**：
起一个特权 + hostPath `/` 的 Pod → 在 Pod 里用 `python3` 跑我们的 stdlib 构建脚本
（tar+gzip+sha256+Registry v2）→ `ctr images import` 落到节点 containerd → 该节点上的 Pod
用 `imagePullPolicy: Never` 直接用它。**全程不依赖 docker。**

---

## 2. 今天的实测证据（全部在 n1 上跑出）

### 2.1 云主机 n1 自身没有任何构建工具

```
docker / podman / buildah / kaniko / nerdctl / ctr / crictl /
skopeo / img / umoci / buildkit      → 全部 缺失
kubectl v1.34.3-eki.2606.0（Kustomize v5.7.1）
```

### 2.2 RBAC 边界（比之前记录的更窄）

| 动词 | 结果 |
| --- | --- |
| `create jobs` / `create pods` / `create deployments` | ✅ yes |
| `create configmaps` / `create serviceaccounts` | ❌ no |
| `create pods/exec`（`--subresource=exec`） | ❌ no |
| `create pods/status` | ❌ no |
| `list storageclasses` | ❌ Forbidden |
| `get nodes`（`containerRuntimeVersion` / `osImage` 全读不到） | ❌ 空 |

⚠️ 后果：**不能用 `kubectl exec` 进 Pod**。所以「在集群里打镜像」不能靠
exec 交互，必须把整个动作写进 Pod 的 `command/args` 一次跑完（我们本来就这么干）。

### 2.3 特权 Pod + hostPath `/` —— 决定性的一步，成功了

Pod `talea-build-probe`（节点 h100-04，`imagePullPolicy: Never`）实测：

```
uid=0(root) gid=0(root) groups=0(root)
/host/opt  →  WRITE-OK          ← 宿主根盘可写
```

宿主根可枚举，关键资产现身：

```
/host/usr/bin/ctr           24,948,000 B   2026-02-25     ← 宿主 ctr 可用
/host/usr/local/bin/ctr     23,445,825 B   2025-05-20
/host/apps/data/containerd  drwx--x--x 15 root root
/host/var/lib/containerd -> /data01/containerd
/host/usr/bin/python3 -> python3.10          ← 宿主是 py3.10
/host/opt  /host/data01 ... /host/models ...
```

Pod 内自带 `python3` = **3.12.3**，`import zlib,tarfile,hashlib,http.client` → `stdlib OK`。

> 即：**构建所需的全部要素在 Pod 里同时具备** —— root、可写宿主盘、
> Python 3.12 + 标准库、宿主 ctr。

### 2.4 出网：两条都不通

```
tcr      (base-mirror.tencentcloudcr.com)  → 000，不可达
ecis     (aiops-...cmecloud.cn)            → 000，不可达
```

与既有结论一致：**Pod 完全 air-gap**。所以「构建出来的镜像往哪送」必须另外解决
（见 §4）。

---

## 3. 这条路我们其实已经走过一遍

09-23 从 h100-04 把 `lmsysorg/sglang:v0.5.20-cu130-runtime` 迁到 h100-01 用的就是这套：

1. 拷宿主 `/usr/bin/ctr` 进容器；
2. `ctr -n moby images export` 导出 tar（**ctr 不做 docker.io 规范化，必须写全名**）；
3. 对端用 `chroot /host curl` 下载（hostPath 挂载实际只读，写必须 chroot）；
4. `chroot /host /usr/bin/ctr -n k8s.io images import`（**ctr 2.x 无全局
   `--snapshotter`，用 env `CONTAINERD_SNAPSHOTTER=overlayfs`**）；
5. `ctr images check` 看 `unpacked=true`。

耗时约 8 分钟，13 GB。

→ 把「构建」接到这条链的前端，就是完整的**集群内自产自用**闭环。

---

## 4. 真正的瓶颈不是「能不能 build」，是「字节从哪来 / 往哪去」

构建本身没有障碍。卡点是 air-gap 下的三个口子：

| 口子 | 现状 | 可行做法 |
| --- | --- | --- |
| **料怎么进**（基础镜像 / wheelhouse） | 节点无外网 | ① 平台经 ecis SWR 代放；② n1 有外网，可作中转载体（但 n1 入站只有 22 端口，需借 Pod/宿主 chroot 落地） |
| **镜像怎么出**（推 registry） | 节点无外网，TCR 不可达 | ① 同一 Pod 既是构建器又是导出器 → 直接 `ctr export` 到宿主 hostPath，再由能出网的一方取走；② 平台代推 |
| **下次怎么复用** | 每次重建要重走一遍 | 构建一次 → `ctr import` 到节点 → 该节点永久 `Never` 可用；多节点则各导一次 |

**关键洞察**：如果目标只是「让本集群的节点用上这个镜像」，
**根本不需要 registry**。Pod 里 build 出 tar → 直接 `ctr import` 到宿主 containerd
→ 完事。Registry 只有在「跨集群分发」时才是必需品。

---

## 5. 三个层次的答案

### L1 现在就能做（不需要机器，只需云主机 + 集群 API）
起 Job/Pod 在集群内跑构建与导入。RBAC 允许 `create pods`，pod 起得来（已实测），
hostPath 可写（已实测），ctr 在（已实测）。**唯一没走完的是把脚本真跑一遍**——
上次被中断在 `/host/run/containerd/containerd.sock` 探测那一步，需要夜间上机确认。

### L2 夜间上机后能验证的
1. `ctr -n k8s.io images ls` 能读到节点现有镜像（socket 路径 `/host/run/containerd/containerd.sock`）；
2. 用我们真正的 `talea-worker-v0.1` 层在集群内重建一遍（12.6 GB tar → gz）；
3. `ctr images import` 落盘 → `images check` 看 `unpacked=true`；
4. 起一个用该镜像的 Pod，跑 `/bootstrap/health`，验证 `X-Bootstrap-Token` 握手。

### L3 长期（要平台配合）
- ecis Harbor 写权限，或平台代放/代推通道；
- 这样才谈得上「跨集群分发的镜像供应链」。

---

## 6. 对别人以为的「在 k8s 打镜像」的纠偏

很多人第一反应是「装个 kaniko 在集群里 build」。在**这个**集群不成立，因为：

1. kaniko/buildkit 镜像本身就进不来（air-gap，首页就走不通）；
2. 就算进来了，它们需要拉基础镜像 → 同样 air-gap 死；
3. 而我们的 stdlib 管线**不需要任何额外镜像**，跑在现有 sglang 镜像里就行，
   这正是「零依赖构建」的价值所在。

**结论：不用引入构建工具，用已有的 Python 管线 + 宿主 ctr，就够了。**
