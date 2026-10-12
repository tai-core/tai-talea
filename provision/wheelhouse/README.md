# 离线运行时制品（wheelhouse）

Talea 的铺装链路**只消费制品**，它不解析依赖、不下载、不编译：

| 位置 | 行为 |
| --- | --- |
| `provision/ssh_install.py` | 只接受**裸文件名且以 `.whl` 结尾**的制品；逐个核对 `manifest.json` 里的 SHA256 之后才上传；`requirements.txt` 单独核对 |
| 容器内 `provision/install_node.py` | `pip install --no-index --find-links=/opt/tai-talea/wheelhouse -r .../requirements.txt`，然后 `pip check` |

所以「从零搭一套能跑的推理环境」缺的那一半在这里：**造一个参考环境，把它冻成可搬运的制品，把制品核一遍再搬。**

```
build-env.sh   联网机器上把参考环境从零搭出来        （造环境）
build.py       把参考环境冻结成离线 wheelhouse        （造制品）
verify.py      制品离手前，把消费端的每条闸门先过一遍  （核制品）
wheels/        公网索引上没有的轮子放这里（内部 fork 的 sglang 等）
spec.json      这三步共用的规格：profile、源、断言
```

## 三条命令

```bash
# 1. 造环境（联网；约 10 分钟，镜像 ~8GB）
./build-env.sh /root/talea-ref

# 2. 造制品（在参考环境所在的机器上；约 10 分钟，制品 ~4GB）
./build.py --venv /root/talea-ref --output ../out

# 3. 核制品（任何地方；纯标准库，不需要网络）
./verify.py --wheelhouse ../out --profile ../../deploy/container/profile.json
```

`build.py` 结束时会直接打印填进控制节点安装清单（`onboarding-manifest.json`）的三个值：

```json
{
  "wheelhouse_source": "<制品目录>",
  "wheel_manifest_sha256": "<manifest.json 的 sha256>",
  "requirements_sha256": "<requirements.txt 的 sha256>"
}
```

清单的完整形状见 [docs/ssh-onboarding.md](../../docs/ssh-onboarding.md)。

## 制品长什么样

```
out/
  manifest.json       files: [{name, size, sha256}]；安装器只读这个
  requirements.txt    pip freeze --all 冻出来的完整闭包
  *.whl               只允许轮子；一个 sdist 都不要
```

`manifest.json` 里除 `files` 之外还记了 profile、参考环境的 pip 版本、spec 的 sha256、
每个轮子的来源（本地覆盖 / 索引 / 由 sdist 编译而来）。安装器忽略这些字段，它们是六个月后
回答「这堆东西到底是哪来的」用的。

## 制品字节一个都不进 git

`sglang==0.5.20` 的**本体只有 27 MiB** —— 但它有 **76 个非可选直接依赖、合计 1.22 GiB**。
实测（2026-10-08：从 wheel 里读 `Requires-Dist`，再逐个取 PyPI 元数据）：

| 轮子 | 大小 |
| --- | --- |
| `torch` | 529 MiB |
| `sglang-kernel` | 354 MiB |
| `tilelang` | 47 MiB |
| `av` | 41 MiB |
| `scipy` | 36 MiB |
| `flashinfer-python` | 32 MiB |
| 其余 70 个加起来 | 约 190 MiB |
| `sglang` 本体 | 27 MiB |

**`torch` 和 `sglang-kernel` 两个就 883 MiB，双双越过 git 的单文件硬上限（100 MiB）。**
所以「把 wheelhouse 编好放进仓库」在物理上做不到 —— 不是不推荐，是 `git push` 会被服务端
直接拒绝。

这里有个容易算错的地方：`torch` 取自 pytorch 的 cu130 索引，**那个构建不声明任何 `nvidia-*`
依赖**（实测闭包 10 个包 0.53 GiB，没有 cudnn / cublas / nccl），它链接基础镜像自带的 CUDA。
「基础镜像已经有了」省掉的就是这一块。要是换成公共 PyPI 的 torch，会再拖进
`nvidia-cudnn-cu13` 586 MiB、`nvidia-nccl-cu13` 291 MiB 这一整套，闭包立刻翻倍还多。

还有第二条、和体积无关的理由：`tai-talea` 是 **public** 仓库，内部 fork 是 **private** 仓库。
那个 fork 编译出来的 sglang 轮子是仓库最该带走的东西，但它恰恰最不该提交 —— 等于把内部补丁
公开。**2026-10-08 用户明确确认过这条边界。**（详见 `wheels/README.md`。）

所以：**仓库带身份，制品库带字节。**

| | 放哪 |
| --- | --- |
| 锁、manifest、spec、脚本、这份文档 | 仓库（就是现在这些） |
| 内部 fork 的 sglang、只能现场造的轮子 | 制品库 / NAS，用 `local_wheels.paths` 指过去 |
| 其余全部（torch / sglang-kernel / triton …） | 不存；`build.py` 按锁一条命令重新下到同一份文件 |

本地槽 `wheels/` 的内容也**不进 git**（`.gitignore` 挡着）。它对 `build.py` 的唯一硬规矩
是**只放裸 `*.whl`** —— 因为铺装时是拿 `*.whl` 去 glob 的，放个 `.tar.gz` 进去既不报错也不
生效，是无声的；`wheelspec.check_local_slot()` 把这种无声变成中止。体积只报告不拦：
`build.py` 结束时会告诉你制品里有几个轮子永远进不了 git。

## 为什么是这些步骤

- **锁来自 `pip freeze --all`，不是解析出来的。** 参考环境是唯一权威；任何解析器都只能
  引入参考环境里没有的版本。
- **下载一律 `--no-deps`。** 锁本身就是闭包，加解析只会带来意外。
- **输出目录里已有的轮子优先。** 内部 fork 的 sglang、以及只能由 `pip wheel` 现场造出来的
  包，放进 `wheels/` 就压过索引。
- **`.whl` 之外一律不留。** 消费端会拒；在这里拒比在容器里拒便宜。
- **带本地版本标签的钉子不做自动降级。** `torch==2.13.0+cu130` 去掉标签后在 PyPI 上能下到
  ——那是**另一份构建**。所以 `indexes.extra` 里始终留着 pytorch 索引，下不到就报错。

## profile：三处必须一致

```
provision/wheelhouse/spec.json          <- 造制品时用的
deploy/container/profile.json           <- 打进镜像、容器自检读的
控制面配置的 image_profile               <- 控制面下发的
```

不一致的后果不是报错，是**容器自检判不兼容、exit 29、实例被释放**。
`verify.py` 会把三者拉到一起比，还会把制品集里每个轮子的 python 标签和 profile 的
python 版本对一遍（一个用 cp310 冻出来的 wheelhouse 在节点上装不出任何东西）。

当前基线：`ubuntu-24.04 / cuda 13.x / python 3.12 / sglang 0.5.20`。
`13.x` 是有意的：阿里云通道的镜像是 13.0.2，移动云 k8s 节点上唯一缓存的通用镜像是
13.1.1，`x` 通配两侧都放行（和 `bootstrap/tai_talea_bootstrap/profile.py` 同一套规则）。

## 制品怎么送到没有出网的平台上

**SSH 铺装（阿里云 CCI 通道）**：制品留在控制节点，`ssh_install.py` 按 SHA256 增量分发。
已缓存且一致的文件直接复用，重试不会重复传 4GB。这条路就是现在在跑的。

**镜像（移动云 k8s 租户）**：那条路禁 exec、禁 port-forward、容器完全无出网，
所以制品必须在镜像里。仓库里的 `deploy/container/Dockerfile` 现在支持直接用制品搭运行时层：

```bash
# 工作目录是仓库根；WHEELHOUSE_DIR 是构建上下文里的相对路径
docker build -f deploy/container/Dockerfile \
  --build-arg BASE_IMAGE=<裸 CUDA 镜像> \
  --build-arg WHEELHOUSE_DIR=provision/wheelhouse/out \
  -t <内网仓库>/talea-worker:<tag> .
```

装运行时层的是 `deploy/container/runtime-layer.sh`：它顺手断言后置条件、写上 cu12 的
loader 配置、补回 JIT 需要的 `lib64`/无版本号符号链接。不传 `WHEELHOUSE_DIR` 时行为不变：
假定基线镜像自带一份已验证的 `/opt/tai-talea/{venv,wheelhouse}`。

**k8s 的 hostPath（没有镜像导入权限时的退路）**：把制品和解包好的 bootstrap 放到宿主机上，
用 `partners[].kubernetes.volumes` 挂进容器：

```yaml
      volumes:
        - name: models
          host_path: /apps/models
          mount_path: /models
          read_only: true
        - name: wheelhouse
          host_path: /apps/talea/wheelhouse
          mount_path: /opt/tai-talea/wheelhouse
          read_only: true
        - name: onboarding
          host_path: /apps/talea/onboarding
          mount_path: /opt/tai-talea/onboarding
          read_only: true
```

⚠️ 这条路只解决了制品，没解决 `install_node.py` 的两步系统动作：`apt-get install` 和
`python3 -m venv`（无出网的镜像缺 `python3-venv`，`ensurepip` 用不了）。
在没有包源的平台上，要么让镜像把这两步做完（上面那条 `docker build` 就是干这个的），
要么把 venv 一起投送。**不要指望容器自己在离线环境里把 venv 建起来。**

## 踩坑清单

这些都是真出现过、并已编进脚本或断言里的：

1. **`ninja` 缺了必崩。** 基础镜像不带，sglang 的 tilelang/EAGLE 路径 JIT 直接
   `FileNotFoundError`。apt 和 wheelhouse 两条路都留着。
2. **mooncake 的公开轮子是 cu12 构建的。** CUDA 13 镜像上 `import mooncake` 缺
   `libcudart.so.12`。制品里带上 `nvidia-cuda-runtime-cu12`，`install_node.py` 把它的
   lib 目录加进 `LD_LIBRARY_PATH`；走镜像那条路则在镜像里写 `ld.so.conf`。
3. **sdist 会被消费端拒。** 消费端只认 `.whl`，所以源码包必须在构建端 `pip wheel` 成轮子
   （`builder.build_sdists`）。
4. **索引漏一条会伪装成「包本身有问题」。** `sglang==0.5.20` 钉了 `cuda-tile==1.6.0rc5`，
   PyPI 上这个版本**只有一份 7 KB 的 sdist**，而它是 `wheel-stub`：真正的轮子在 NVIDIA 自己
   的索引上（`pypi.nvidia.com` 会 301 到 `pypi.nvidia.cn`）。少写 `indexes.extra` 里那一条，
   `pip install sglang==0.5.20` 会死在 `No matching distribution found for
   cuda-tile==1.6.0rc5` —— 报错指向 sglang，根因却在索引配置。**不要**为此去现场编译，加索引
   就完了。
5. **别把 `abi3` 轮子当成 ABI 不匹配。** `sglang-kernel`（354 MiB，制品里第二大的东西）和
   `tilelang`、`tokenizers` 都用稳定 ABI：文件名是 `cp310-abi3`，在 cp312 上完全合法。
   `verify.py` 的 `python_tag_ok` 只对 `abi3` 放宽**向前**兼容（`cp313-abi3` 进不了 3.12 的
   venv），普通的 `cp310-cp310` 依然要求精确匹配。
6. **`--python-version` + `--platform` 交叉下载需要 `--only-binary`。** 所以交叉模式下
   没有 sdist 可言；能原生下就原生下（`build.py` 会用 profile.python 提醒你）。
7. **参考环境不全 → 事后手工补轮子。** 2026-09-30 那次就往 wheelhouse 里手工补了
   `runai-model-streamer` 和 `humanize`，再重算两个哈希、再改控制节点的安装清单。
   现在这两样进了参考环境的合同（`requirements.top`），`build.py` 会挡住偷工减料的参考环境。
8. **工具链目录布局。** pip 装的 CUDA 轮子是 `nvidia/cu13/lib`，sglang 的 JIT 按
   `lib64` 和无版本号 `.so` 找库。`build-env.sh` 第 9 步补这些符号链接，少了它报的是
   「找不到库」而不是「库版本不对」。

## 测试

```bash
make test-provision     # 含 provision/tests/test_wheelhouse.py
```

`test_wheelhouse.py` 用假的 pip 把 `build.py -> verify.py` 整条跑通，并逐个验证
篡改字节、缺件、混进 sdist、混进错 ABI 的轮子、profile 不一致这五种情况都会被拒。
