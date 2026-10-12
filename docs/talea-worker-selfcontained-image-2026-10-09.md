# talea-worker 自足镜像：构建 → 推送 → 集群自动拉取 → 运行（2026-10-09）

## 1. 产物

```
shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn/library/talea-worker:v0.1
  manifest   sha256:08acea33ae153a50d4b4eebf65298d37c544cedb38605859b7d45eb571b51dc9
  layer      sha256:8564bd2b6ce7496af5b1bf1d824d7893ae1a462665a262b14a14741354b41d90
             gz 12.16 GiB (13,058,727,860 B)  /  raw 21.60 GiB (23,190,640,640 B)
  diff_id    sha256:fdf95cebac3bd07499f7ea7f9dfe64c02080d16d54980cc7b803e0f5827d1308
```

内容：**Ubuntu 24.04 + CUDA 13.0 工具链 + `/opt/tai-talea` 运行时 bundle**
（venv 9.4 G / sglang 0.5.20 / torch 2.13.0+cu130 / wheelhouse 3.9 G / bootstrap / entrypoint / profile.json）。

与 `talea-worker-v0.1`（TCR，只含 `/opt/tai-talea` 的**非自足** bundle 层）的关键区别：
这一个**可以直接当 Pod 镜像跑**，不需要再叠加 partner 基础镜像。

### 为什么自足版反而更小

曾考虑"base 镜像 + bundle 层"的合成方案，算下来**更大**：`lmsysorg/sglang:v0.5.20-cu130-runtime`
和我们的 venv 都各自带一套 torch/CUDA，而 OCI 层之间**无法去重**，合成后约 18 GiB。
一次性快照整个 rootfs 只有 12.16 GiB。所以选自足。

---

## 2. 构建方法（无 docker）

脚本 `.workbuddy/tmp/h100-glm53/talea-worker-full-image.py`（纯 Python 标准库，453 行）：
流式 `tar` → `zlib.compressobj(5, DEFLATED, 31)` → gzip 层，边打边算 `diff_id` 与 layer digest，
再走 Registry v2（`POST` 发起 → 分块 `PATCH` 64 MiB → `PUT` 收尾）推 config blob + layer blob + manifest。

运行在九章 prefill 容器（该容器无 `CAP_SYS_ADMIN`，没有 dockerd）。

### 两条排除策略

| 机制 | 作用 |
| --- | --- |
| **`st_dev` 挂载点守卫**（主） | 越过任何 `st_dev` 与 `/` 不同的条目 → 自动丢掉 867 G 的 `/anc-init`、70 T 的 `/root/public`、1.2 P 的 `/root/gongji`，以及 bind 进来的 NVIDIA 驱动 `.so`。本次跳过 52 项 |
| 路径黑名单（辅） | 显式排除 `/root`、`/proc`、`/sys`、`/dev`、`/run`、`/tmp`、`/package`、`/command`、`/code`、`/etc/{hosts,hostname,resolv.conf,machine-id}` |

### 三次迭代（教训都在这里）

| # | 结果 | 问题 |
| --- | --- | --- |
| 1 | raw 38.60 GiB → gz **21.25 GiB** | `/root` 下有**别人留下的 17+ GiB**（`whb-venv` 9.4 G、`wheelhouse` 3.9 G、`sglang-router` 3.0 G、Rust 工具链 1.2 G）全被打进镜像 |
| 2 | raw 21.60 GiB → gz **12.16 GiB** | 排除整个 `/root`。体积降 43%，耗时 415 s |
| 3 | 同尺寸，**可运行** | 集群验证暴露两个真实缺陷，见下节 |

### 第 3 次暴露并修掉的两个缺陷

1. **缺 `/tmp`** —— `/tmp` 被整个排除，`runc` **不会**自动创建它，导致 entrypoint 里
   `> /tmp/serve.log` 失败、bootstrap 根本没起来。
   → 修复：新增 `EXTRA_DIRS`，把 `/tmp`(1777)、`/var/tmp`(1777)、`/run`(755)、`/root`(700)
   作为**空目录条目**补进层里。
2. **`profile` 自检 cuda 不匹配** —— `_detect_cuda()` 先读 `/usr/local/cuda/version.txt`，
   但 **CUDA 13 只有 `version.json`、没有 `version.txt`**（宿主上这个文件本来就不存在），
   于是回退 `shutil.which("nvcc")`；而镜像 config 的 PATH 里**没有 `/usr/local/cuda/bin`**
   → `cuda=""` → 不匹配 `13.x` → **容器自检 exit 29、实例被释放**。
   → 修复：PATH 前面插入 `/usr/local/cuda/bin`，`nvcc --version` 返回 `release 13.0` 即匹配通配 `13.x`。

---

## 3. 集群端到端验证（nightshift / h100-04）

### 3.1 kubelet 自动拉取

```yaml
image: shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn/library/talea-worker:v0.1
imagePullPolicy: Always      # 必须是 Always，节点上本来没有这个镜像
```

结果 —— **`imageID` 的 digest 与推送的 manifest digest 完全一致**：

```
…/library/talea-worker@sha256:08acea33ae153a50d4b4eebf65298d37c544cedb38605859b7d45eb571b51dc9
```

kubelet 日志：`firstStartedPulling=22:00:25 → lastFinishedPulling=22:07:09`，**约 7 分钟**拉完 12 GiB。

> 排障插曲：中途查 `ingest` 目录为空、content store 12 秒无增长，一度以为卡死。
> 实际是拉取正在进行、只是采样窗口太短。**判断拉取是否活着，要看 kubelet 的
> `Observed pod startup duration` 那行（含 `firstStartedPulling` / `lastFinishedPulling`），
> 不要只看 `ingest` 目录**。读法：特权 Pod + `chroot /host /usr/bin/journalctl -u kubelet -n 60`。

### 3.2 运行时自检（Pod 内实测输出）

```
1. OS        VERSION_ID="24.04"  ID=ubuntu        kernel 5.15.0-119-generic
2. identity  uid=0(root)  workdir=/opt/tai-talea
3. dirs      /tmp /var/tmp /run /root 均存在        ← 修复 1 生效
4. python    Python 3.12.3
5. cuda      /usr/local/cuda/bin/nvcc → release 13.0, V13.0.88   ← 修复 2 生效
6. sglang    sglang 0.5.20 / torch 2.13.0+cu130 cuda 13.0
7. selfcheck SELFCHECK_OK {"os":"ubuntu-24.04","cuda":"13.0","python":"3.12",
                            "sglang":"0.5.20", …}          ← 不会 exit 29
```

### 3.3 bootstrap 服务真实启动

```
{"msg":"serving bootstrap tai-talea-bootstrap/1 on 0.0.0.0:9001 (shared secret required)"}
{"msg":"127.0.0.1 - \"GET /bootstrap/health HTTP/1.1\" 200 -"}
$ entrypoint.sh health
{"status": "ok", "phase": "IDLE", "detail": "bootstrap is up", "version": "tai-talea-bootstrap/1"}
HEALTH_OK
```

### 3.4 生产形态（不覆盖 command，只给 image + token + readiness 探针）

Pod `talea-worker-service`：**1/1 Running**，24 秒达到 Ready（镜像已在节点缓存）。

readiness 探针（talea partner 的实际机制）由 kubelet 从 `10.214.81.70` 持续打到容器 `:9001`：

```
10.214.81.70 - "GET /bootstrap/health HTTP/1.1" 200 -
```

即：**只要给镜像地址 + `TAI_TALEA_BOOTSTRAP_TOKEN`，talea 的 kubernetes partner 就能把它拉起并判活。**

---

## 4. 全链路时延基线

| 阶段 | 耗时 | 备注 |
| --- | --- | --- |
| build（九章，21.6 GiB raw，单线程 zlib-5） | **415 s** | `/dev/shm` 落 layer.gz（根盘仅余 14 G，`/dev/shm` 574 G） |
| push（九章 → ecis，IPv6） | **320 s @ 39 MB/s** | 12.16 GiB |
| 节点 kubelet 拉取（内网 → 192.168.203.110） | **~420 s** | 12 GiB，约 30 MB/s |

镜像推上去后，**同一个 Pod 重建只需 24 秒**（节点已缓存）。

---

## 5. 与 talea 自动化的关系

这一条闭环补掉的正是缺口清单里的 **M2（worker bundle 供给）**，并且把供应路径
从"每节点 `ctr export/import` 手工搬运 13 GB"变成 **push 一次、kubelet 自动拉**。

剩下未闭环的：

- **h100-01 仍缺 ecis 的 `/etc/hosts` 映射**（且当前 RBAC 无法给它调度 Pod 去补），
  只能夜间上机或请平台统一配。
- **`imagePullPolicy` 必须是 `Always`**，因为节点上没有预置该镜像；partner 的
  `manifest.go` 目前默认渲染什么策略需要核对。
- **镜像 12.16 GiB / 解包 21.6 GiB** 是"全盘快照"的代价。若后续要压缩到 3~4 GiB 量级，
  方向是拆成"基础层（OS+CUDA，可复用官方镜像）+ bundle 层"，但那需要 registry 侧
  支持跨仓库 blob 引用（或接受一次性把基础层也推上去）。

---

## 6. 用法

```bash
# 拉取
docker pull shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn/library/talea-worker:v0.1

# 集群里跑（nightshift 示例）
kubectl apply -f verify-worker.yaml        # 一次性自检
kubectl apply -f service-worker.yaml       # 常驻，带 readiness 探针

# 遗留的活实例（本次验证留下的）
kubectl get pod talea-worker-service -n nightshift
kubectl delete pod talea-worker-service -n nightshift
```

相关文件：

- 构建脚本 `talea-worker-full-image.py`（纯 stdlib）
- 自检清单 `verify-worker.yaml` / 生产形态 `service-worker.yaml`
- 验证原始输出 `talea-worker-cluster-verify-20261009.log`
- 前一版非自足 bundle：`tai-talea/docs/talea-worker-image-2026-10-09.md`
- 入城通道实测：`tai-talea/docs/ecis-registry-probe-2026-10-09.md`
