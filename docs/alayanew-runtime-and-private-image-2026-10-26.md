# 九章智算云：GLM-5.3-Flash 运行时契约与私有镜像边界（2026-10-26）

## 0. 结论速览

| # | 事实 | 依据 |
| --- | --- | --- |
| 1 | 镜像自带的 sglang **加载不了这个模型** | 镜像 `alayanew-public/sglang:20250930` 的两棵源码树都只有 `glm4*`/`chatglm`，无 `glm5_next.py` |
| 2 | 能加载它的是 **venv 里的 sglang 0.5.22** | `srt/models/glm5_next.py` 存在，`Glm5NextForConditionalGeneration` 可导入 |
| 3 | 运行时可以**烤到共享盘**，2 分钟铺装 | `/root/gongji/tai-talea-runtime/*.tar` + `cci-unpack-runtime.sh` |
| 4 | 九章的 `save-image` 对**未开通镜像仓库的租户不可用** | `{"status":100045,"message":"请求镜像仓库失败"}` |
| 5 | 平台**支持私有镜像**，但门槛不在我们的代码里 | 控制台「镜像类型」有「私有镜像」，创建接口无仓库凭据字段 |
| 6 | `imageType` 的文档出处是**训练任务**接口，不是 CCI | 见 §4 |

---

## 1. 为什么必须换 sglang

模型目录 `/root/gongji/GLM-5.3-Flash`：62 shard / 305.8 GiB、`model_type=glm5_next`、
`architectures=['Glm5NextForConditionalGeneration']`、**没有 `auto_map`、没有任何 `*.py`**。

所以 `--trust-remote-code` 在这里是空操作 —— 没有远程代码可执行，模型类只能来自
sglang 自己的 `srt/models/`。

镜像里 `import sglang` 实际解析到 `/sgl-workspace/sglang-private`（editable finder，
`PYTHONPATH` 为空），两棵树的 `models/` 目录都**没有** `glm5_next.py`：

```
/sgl-workspace/sglang/python/sglang/srt/models/         -> glm4*, chatglm
/sgl-workspace/sglang-private/python/sglang/srt/models/ -> glm4*, chatglm
```

失败形态是 `sglang exited unexpectedly with code 2`，日志里是 argparse 之后立刻退出，
没有任何「模型不认识」的字样 —— 这是最初误判为「参数写错」的原因。

**九章上次跑通靠的是容器内装的 community sglang 0.5.20**，不是镜像自带。
证据是 `jz-tai-talea-v2.yaml`（10-12 19:52）的 `image_profile.sglang: "0.5.20"`
与文件头注释。

## 2. 运行时的两条落地路线

### 2.1 私有镜像（推荐，当前被平台侧挡住）

见 §4。

### 2.2 共享盘 tar 包（当前可用）

`cci-venv-0522.sh` 建 `/opt/tai-talea/venv`（sglang 0.5.22 + torch 2.14.1+cu130），
`cci-bake-runtime.sh` 把它与 bootstrap 源码、JIT 缓存打成三份 tar 放到
`/root/gongji/tai-talea-runtime/`（CephFS，容器释放后仍在）：

| 产物 | 大小 | 作用 |
| --- | --- | --- |
| `venv-0.5.22-cu130.tar` | 11.2 GB | 解释器 + sglang + torch |
| `tai-talea-bits.tar` | 27.7 MB | `/opt/tai-talea`（除 venv）+ `/etc/tai-talea` |
| `jit-caches.tar` | 122 MB | DeepGEMM / tilelang / flashinfer / triton 编译缓存 |

新容器跑 `cci-unpack-runtime.sh`，**实测 1 分 46 秒**（对比 25 分钟 PyPI 下载）。

⚠️ 脚本的 `venv_ready()` 判据是「能否 import glm5_next」，**不是「目录存在」**：
基础镜像本来就带一个 venv（0.5.3rc0），按存在判断会静默复用错的那一个。

⚠️ `tar --exclude` 在 create 模式下**位置敏感**：写在成员路径之后会静默失效，
`bits.tar` 会变成 11 GB（把 venv 又打一遍）。

## 3. 依赖与硬件

| 组件 | 0.5.20 | 0.5.22（现役） |
| --- | --- | --- |
| torch | 2.13.0 | 2.14.1+cu130 |
| flashinfer_python[cu13] | 0.6.18 | 0.7.1 |
| transformers | 5.12.1 | 5.19.0 |
| sglang-kernel | 0.4.7 | 0.4.9 |

节点是 8×H100 80GB（SKU 名写 `NVIDIA-H800A-NV-80G`），驱动 580.173.02 / CUDA 13.0，
镜像 nvcc 12.9。**CUDA 13 的轮子能跑**：运行看驱动，只有编译期才看 nvcc。
`image_profile.cuda` 仍然写 `12.9`，因为 `profile._detect_cuda()` 读的是 nvcc 不是驱动。

`--disaggregation-mode` 强制依赖 Mooncake：`init_parallel_runtime` 会走
`maybe_init_shared_mooncake_transfer_engine`，所以 venv 必须有
`mooncake-transfer-engine`（实测 0.3.13.post1）。容器有 `ib7s400p0..p7`，
默认 `mooncake` 后端自动探测 RDMA 成功（`Using RDMA transport (RoCE/iWARP)`）。

## 4. 私有镜像：平台支持，但有前置条件

### 4.1 平台怎么说

- 控制台创建 CCI：「Image（镜像类型）……支持`基础镜像`、`应用镜像`和私有镜像三种类型」
- CCI API 总览：「自定义镜像：支持私有镜像仓库与实例镜像保存」
- 但 **CCI 创建接口在文档站没有独立页面**，sitemap 里只有 `cci/list`、`cci/get-instance`
  这些读接口，没有 create。

`imageType`（`general|application|private`，需与 `image` 对应）**唯一的文档出处是
分布式训练任务接口** `POST /v1/subscription/training/create`。同一份文档的示例请求里
出现了租户级仓库路径：

```json
"imageType": "private",
"image": "harbor.unite.zetyun.cn/alayanew-87cbf7ca-f126-4194-b137-8fde5adcd09b/fq-torch-1120:latest"
```

即租户镜像仓库的项目名形如 `alayanew-<租户 UUID>`，不是任意名字。

### 4.2 为什么现在打不了

| 探测 | 结果 |
| --- | --- |
| `POST /cci/instance/{id}/save-image` | `{"status":100045,"message":"请求镜像仓库失败"}` |
| Harbor 项目列表 | 28 个项目，**没有租户项目**（只有 `alayanew-public`、`cci-system`、`alab` 等） |
| 匿名取 `tai-talea/sglang-glm53:pull,push` token | 签发成功，但 JWT `access[].actions` 是**空数组** |
| `PUT /v2/tai-talea/sglang-glm53/manifests/probe` | `401 {"message":"project tai-talea not found"}` |

注意 Harbor 的 token 端点是 `/service/token`（从 `/v2/` 的 `WWW-Authenticate` 拿到），
不是 `/v2/token`；而且**未授权的 scope 也会签发 token**，必须解码 JWT 看
`access[].actions` 才能判断真实权限 —— 只看「拿到 token 了」会得出相反结论。

### 4.3 代码侧已经就绪

`internal/partners/alayanew/adapter.go` 的 `Config` 增加 `ImageType`，
`CreateInstance` 在非空时下发 `cciParams["imageType"]`，`New()` 对取值做白名单校验
（拼错在配置加载期就失败，而不是创建时给出无法定位的错误）。

等平台开通「混闪-镜像」、拿到项目名之后，只需要：

```yaml
image: registry.hd-02.alayanew.com:8443/<项目>/sglang-glm53:0.5.22-cu130
image_type: private
```

容器起来就自带 sglang 0.5.22，不必再跑解包脚本。

## 5. 踩过的坑（都是可复现的）

### 5.1 `--model-loader-extra-config` 的引号会被 shlex 吃掉

bootstrap 的 `validate_extra_args` 对每个条目做 `shlex.split`（`roles.py:125`），
所以配置里写

```yaml
- '--model-loader-extra-config={"concurrency":16}'
```

到 sglang 手上变成 `{concurrency:16}`，`orjson` 在 20 秒后报
`unexpected character, expected a string key: line 1 column 2 (char 1)` ——
一次启动尝试就这么没了。正确写法是让单引号保护双引号：

```yaml
- "--model-loader-extra-config='{\"concurrency\":16}'"
```

`shlex.split` 去掉单引号、保留双引号，sglang 拿到合法 JSON。

### 5.2 控制面在启动途中被重启，bootstrap 的进程会变成孤儿

控制面把 `STARTING` 的所有权记在内存里。重启它 → 该实例变成
`start operation was interrupted: no owner for STARTING state`，
**但 bootstrap 那边的 sglang 还在正常加载**。结果是「控制面认为失败、进程在跑」的两份真相。

判定方法：直接看容器的 `ps` 与 `/opt/tai-talea/logs/sglang-*.log`，不要只信控制面状态。

### 5.3 并发加载会互相抢 CephFS 带宽

`/root/gongji` 是 CephFS HDD 池，单流 ~373 MB/s，`concurrency=16` 能吃到 ~317 MB/s
（≈85%，加到 32 无用）。两台同时加载时总吞吐基本不变，所以**单台 17 分钟的加载，
两台并发要 40 分钟以上**。要最快到「两台都能服务」，串行反而更快。

### 5.4 `pkill -f` 会打到承载命令的 shell 自己

`cci-bootstrap-start.sh` 里用 `pgrep -f "tai_talea_bootstrap serve"` 找旧进程，
在「命令本身包含这个字符串」的调用方式下会匹配到自己，`kill` 掉launcher，
脚本静默什么都不做。改成按 PID（`ps -eo pid,cmd | grep | awk`）或走 wrapper 脚本。

### 5.5 后台进程必须走 wrapper + 立即返回

`jz.py run "cmd &"` 这种形式，SSH 会话结束时子进程随进程组一起被回收，
日志为空 —— 看起来像崩溃，其实是信号。写成磁盘上的脚本、里面 `setsid nohup` 再返回。

### 5.6 重启控制面时 SQLite 会短暂锁住

`pkill` 后立刻起新进程，启动恢复阶段报
`list instances: context deadline exceeded`，随后自愈（查询耗时 <1ms）。
等 3 秒不够时它会自己恢复，不必干预。

## 6. 当前线上形态

| 角色 | 容器 | 内网地址 | 说明 |
| --- | --- | --- | --- |
| 控制面 + Router | `glm53-pd2` | 172.19.166.190 | `tai-talea 0.9.0-cci` / `sgl-model-gateway 0.3.2`，Router 8000 + 控制面 8787 |
| worker | 两台各一 | pd3 = 172.19.232.225 | 每台 8 卡、`gpu_count: 8` |

`1P1D` 必须**两个节点都空**：一台 worker 吃整节点 8 卡，任一台被占就是 0P1D。
