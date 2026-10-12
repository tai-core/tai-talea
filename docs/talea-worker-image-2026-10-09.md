# talea-worker 镜像产出记录（2026-10-09）

在九章（alayanew CCI）prefill 容器上，从既有的 `/opt/tai-talea` 运行时树直接产出一个
可直接被 kubernetes partner 使用的 worker 镜像，并推送到腾讯云 TCR。
**全程无 docker**（容器无 CAP_SYS_ADMIN），仅用 Python 标准库完成 OCI 构建 + Registry v2 推送。

---

## 1. 产物

| 项 | 值 |
| --- | --- |
| 镜像 | `base-mirror.tencentcloudcr.com/mode-optimization/glm53-h100:talea-worker-v0.1` |
| manifest digest | `sha256:1cd1ec54a0ba83ca80fb0c98017fe6c9c4de3d5e5a5f4c003506db5e9b09a924` |
| layer digest | `sha256:27dd544a62780e6bb02eaad3fc6ad5876234be51bc7e191e0b0976e6e1a34c3f` |
| diff_id (uncompressed) | `sha256:01748ca8cd12706a721dee6fc0e79d15b7b9e2b602b8c786440236ed76440b3b` |
| layer size | 8,284,401,439 B（7.72 GiB gz / 12.59 GiB raw） |
| 构建脚本 | `.workbuddy/tmp/h100-glm53/talea-worker-image.py` |
| 构建+推送日志 | `.workbuddy/tmp/h100-glm53/talea-worker-build-20261009.log` |

同仓库下 09-23 的 `v0.1`（11.2 GiB，非 talea bundle）仍然存在，两者不冲突。

## 2. 镜像里有什么 / 没有什么

**有**（layer 顶层，共 75,286 条路径）：

```
./venv/                 75,053 条   Python 3.12.3 venv，9.4 G
./wheelhouse/              212 条   210 个 .whl，3.9 G
./tai_talea_bootstrap/       9 条   bootstrap 服务端包（已安装）
./bootstrap/                 5 条   源码 + 测试
./provision/                 5 条   sglang_compat.py 等铺装脚本
./entrypoint.sh              1 条   6324 B，可执行
./profile.json               1 条   基线身份声明
```

**没有**（刻意 EXCLUDE，构建时已确认不在顶层）：

| 排除项 | 原因 |
| --- | --- |
| `logs/` | 运行时日志（构建时 110 M） |
| `run/` | bootstrap token / pid 文件，属实例状态 |
| `bootstrap-src/` | SSH 铺装用的源码副本；实际运行的是 `tai_talea_bootstrap` |
| `provision-venv/` | 铺装工具的辅助 venv（41 M） |
| `onboarding/` | 每实例 onboarding 状态 |
| `__pycache__` / `.pyc` / `.git` | 构建副产物 |

**也没有 OS**：镜像刻意不自足。基础系统是 partner 的 Ubuntu 24.04 + CUDA 13，
模型权重走 hostPath（306 G > 节点根盘 49 G）。

## 3. 基线身份（与 nightshift 契约对齐）

`/opt/tai-talea/profile.json`：

```json
{
  "os": "ubuntu-24.04",
  "cuda": "13.x",
  "python": "3.12",
  "sglang": "0.5.20",
  "wheelhouse": "/opt/tai-talea/wheelhouse",
  "virtualenv": "/opt/tai-talea/venv",
  "bootstrap_version": "tai-talea-bootstrap/1",
  "model_id": "zai-org/GLM-5.3-Flash"
}
```

与 `deploy/kubernetes/tai-talea.yaml` 的 `image_profile` **逐字段一致**，
也与控制面 `controller.image_profile` 一致 → 容器自检不会 exit 29。

实测支撑（九章 prefill 容器本体）：

| 字段 | 实测 |
| --- | --- |
| os | Ubuntu 24.04.3 LTS |
| cuda | toolkit 13.0.88，driver 580.173.02 |
| python | 3.12.3 |
| sglang | 0.5.20 |
| GPU | 8×H100，`torch.cuda.get_device_capability(0) == (9, 0)` |

## 4. 容器 config

```json
{
  "Env": [
    "PATH=/opt/tai-talea/venv/bin:...",
    "TAI_TALEA_ROOT=/opt/tai-talea",
    "TAI_TALEA_PROFILE=/opt/tai-talea/profile.json",
    "TAI_TALEA_PYTHON=/opt/tai-talea/venv/bin/python",
    "LD_LIBRARY_PATH=/usr/local/cuda/lib64:/usr/local/nvidia/lib:/usr/local/nvidia/lib64",
    "NVIDIA_VISIBLE_DEVICES=all",
    "NVIDIA_DRIVER_CAPABILITIES=compute,utility"
  ],
  "Cmd": ["/opt/tai-talea/entrypoint.sh", "serve"],
  "WorkingDir": "/opt/tai-talea"
}
```

`/opt/tai-talea` 正是 entrypoint 与 `deploy/container` 契约期望的路径 →
**镜像可直接被 kubernetes partner 使用，无需重新打包**。

## 5. 验证（三层，全部通过）

### 5.1 表面对齐 —— 已推送并可在 registry 解析

```
GET /v2/mode-optimization/glm53-h100/manifests/talea-worker-v0.1 → 200
Docker-Content-Digest: sha256:1cd1ec54a0ba83ca80fb0c98017fe6c9c4de3d5e5a5f4c003506db5e9b09a924
tags/list → ["talea-worker-v0.1", "v0.1"]
```

### 5.2 字节完整性 —— 回拉校验 MATCH

从 TCR 重新拉取 layer 并逐字节 sha256：

```
expect sha256:27dd544a6278...  8284401439
got    sha256:27dd544a6278...  8284401439
MATCH
```

回拉速度 78–93 MB/s（比上传的 27 MB/s 快，因为 Harbor 把 blob 放 COS，
GET 返回 307 重定向到 `tcr-ppedo376-*.cos.ap-beijing.myqcloud.com` 预签名 URL，
走 CDN 加速）。

> ⚠️ 排障要点：Harbor 的 `GET /v2/.../blobs/<digest>` 返回 **307**，
> `http.client` / 裸 Registry 客户端若不跟随重定向会静默拿到 **0 字节**
> （sha256 为 `e3b0c442...`，即空串哈希），看起来像「推送失败」。
> 必须跟随 `Location`。`ctr`/`docker`/`containerd` 原生处理该重定向，不受影响。

### 5.3 内容自洽 —— 解包即用

把 layer.gz 完整解到 `/dev/shm/talea-worker-image/rootfs`（13 G，65 秒），
用其中的 venv 跑真实导入：

```
py 3.12.3  prefix /opt/tai-talea/venv
sglang 0.5.20      torch 2.13.0+cu130  cuda 13.0

  import OK  zmq / sglang / torch / mooncake / tilelang /
             deep_gemm / flashinfer / uvicorn / fastapi /
             transformers 5.12.1 / numpy 2.3.5 / psutil / aiohttp
  import FAIL ray   ← 预期：sglang 0.5.20 用 multiprocessing/uvicorn，不需要 ray

pip check → No broken requirements found.
```

契约路径逐项命中：`./venv/bin/python`、`./entrypoint.sh`、`./profile.json`、
`./wheelhouse`、`./tai_talea_bootstrap`。

## 6. ⚠️ 使用前提：`venv` 依赖宿主 `/usr/bin/python3`

`venv/bin/python` 是符号链接链：`python → python3 → /usr/bin/python3`，
且 `pyvenv.cfg` 声明：

```
home = /usr/bin
include-system-site-packages = false
version = 3.12.3
executable = /usr/bin/python3.12
command = /usr/bin/python3 -m venv /opt/tai-talea/venv
```

含义：镜像**不能**当独立运行时用，宿主必须提供 **Python 3.12 的 `/usr/bin/python3`**。
partner 的 Ubuntu 24.04 基线（系统 python3 就是 3.12.3）满足。
若基础镜像换版本（例如 24.04 之外的 Ubuntu），venv 会因 minor 版本不匹配而失效。

## 7. 复现

```bash
# 1) 脚本上传（SFTP 在本沙箱报 ENOENT，走 base64）
B=$(base64 -w0 talea-worker-image.py)
python ssh-run.py prefill "echo '$B' | base64 -d > /opt/talea-worker-image.py"

# 2) 构建（layer.gz 落 /dev/shm —— 根盘只剩 14G，/dev/shm 有 574G）
python ssh-run.py prefill \
  "cd /opt; setsid nohup python3 -u /opt/talea-worker-image.py build \
   > /dev/shm/talea-worker-image/build.log 2>&1 < /dev/null & disown"

# 3) 推送
python ssh-run.py prefill \
  "cd /opt; setsid nohup python3 -u /opt/talea-worker-image.py push \
   > /dev/shm/talea-worker-image/push.log 2>&1 < /dev/null & disown"
```

实测耗时：构建 ~5.5 分钟（12.59 GiB raw，单线程 zlib level 5），推送 ~5.9 分钟（27 MB/s）。

## 8. 这解决了什么 / 没解决什么

**解决了**：M2「worker bundle 供给」里的**构建侧**——
`.workbuddy/tmp/alayanew/nightshift-automation-gap-2026-10-09.md` §3 里
「talea bundle 从未打包成镜像」这一条现在关闭。
镜像里同时具备 bootstrap 服务端、venv、wheelhouse、entrypoint、基线声明，
路径与 partner 契约一致。

**没解决**（M2 剩余部分，仍是硬缺口）：

1. **进城通道未验证。** nightshift 节点是 air-gap，唯一双向交集是山西 ecis Harbor
   （匿名仅 pull，且**只有 pull 权限**）。本镜像在腾讯云 TCR 上，
   节点够不着。进城路径二选一，都未走通：
   - 平台经 ecis SWR 代放（需平台侧动作）
   - n1 中转 + `ctr images export/import` tar（已验证过 sglang 0.5.20 走这条，
     但对本镜像未验证；且 n1 到节点的唯一通口是 6443，需借 Pod/宿主 chroot 落盘）
2. **节点实时 pull 从未成功过。** nightshift 所有历史 Pod 都是 `imagePullPolicy: Never`，
   即镜像必须先落在节点 containerd 里。本镜像也不例外。
3. **`imagePullPolicy` / registry 凭据**在 partner adapter 里是动态字段，尚未配。
4. **基础镜像的 `dlopen` 面未知。** 本层不含 CUDA runtime 库，
   若宿主基础镜像的 `libcudart` / `libcublas` 与构建机不一致，
   sglang JIT 会在运行时才暴露问题。构建机的 CUDA 是 13.0.88。

## 9. 下一步候选

| 优先级 | 动作 | 说明 |
| --- | --- | --- |
| 高 | 在 h100-04 上做一次「进城 + 起容器」闭环 | 走 ctr tar 通道，把 `talea-worker-v0.1` 落到节点，用 `--entrypoint` 起一个只跑 `/bootstrap/health` 的 Pod，验证 `X-Bootstrap-Token` 握手 |
| 高 | 验证 partner 侧 `imagePullPolicy: IfNotPresent` + 双架构基础镜像 | 关系到「平台直接 pull」这条路能不能走通 |
| 中 | 把 layer 从 7.72 GiB 压小 | wheelhouse 里 210 个 whl 已是压缩态，收益有限；可考虑改成按需拉取或删减到夜班实际用到的子集 |
| 中 | 用真正的 partner 基础镜像重建 | 当前构建机不是 partner 镜像，`dlopen` 面有理论风险 |
