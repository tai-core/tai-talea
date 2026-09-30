# GLM-5.3-Flash 在 H100 上的推理验证结果

日期：2026-09-23（Asia/Shanghai）
承接：`docs/glm53-h100-handoff-2026-09-23.md`（该文档描述的是接手前的失败状态，本文是接手后的结果）

## 1. 结论

**推理验证已完成。** GLM-5.3-Flash（FP8，320B MoE / 18B 激活）在单节点 8×H100 80GB 上以 TP8 成功加载并对外提供 OpenAI 兼容服务。

| 项目 | 结果 |
| --- | --- |
| 服务就绪 | `GET /health` → 200，日志出现 `The server is fired up and ready to roll!` |
| 短对话 | 229 token / 2.15 s，`finish_reason=stop`，内容合理 |
| 长生成 | 512 token / 4.65 s，端到端 **110.1 token/s** |
| 解码稳定吞吐 | **113.4 token/s**（单流，日志连续采样） |
| 权重加载 | 62 分片 / 306 GB，**23.81 s**（`/dev/shm` 内存盘） |
| 单卡显存 | 权重 38.00 GB；含 KV/运行时 ~67.7 GB（80 GB 卡） |
| 启动到就绪 | `tokenizer_e2e=103.98 s`（含 CUDA graph 捕获 20.36 s） |

对比接手时的那份配置（`glm53-start.sh` + `--disable-cuda-graph`）只有 **9.0 token/s**，按官方 Hopper 配方调整后提升 **12.6 倍**。

## 2. 运行环境

- 容器：`root@120.220.102.21:30027`，8×H100 80GB，驱动 580.173.02
- 模型：`/dev/shm/GLM-5.3-Flash`，62 个分片、306 GB、FP8 `e4m3`（`weight_block_size=[128,128]`），架构 `Glm5NextForConditionalGeneration`
- 上游源码：`/dev/shm/sglang-main-src`，editable 安装进 `/dev/shm/glm53-venv`；含 `sglang/srt/configs/glm5_next.py`、`sglang/srt/models/glm5_next.py` 与 `speculative/dflash_utils.py`，即已含 v0.5.20 起的 GLM-5.3-Flash 支持
- Python 3.10.12 / torch 2.13.0+cu130 / flashinfer 0.6.18 / tilelang 0.1.12 / deep_gemm 0.2.0 / sgl_kernel 0.4.7
- **容器内没有 docker**，因此官方镜像 `lmsysorg/sglang:latest` 这条路走不通，只能在 venv 里跑上游源码

## 3. 接手后定位到的 5 个阻塞点（按出现顺序）

前 4 个都属于同一类问题：**这台机器的 CUDA 工具链是由多个互不匹配的 pip 包拼起来的**。

1. **`libnvJitLink.so.13` 找不到** → `import torch` 直接失败。
   库其实存在，问题是 LD_LIBRARY_PATH 没传进进程。修法：把环境变量写进容器内的 `.sh` 文件再 `source`，
   而不是塞在多层 SSH 引号里。
2. **`ninja` 不在 PATH** → sglang 的 JIT 内核构建 `FileNotFoundError: 'ninja'`。
   机器上其实有（`talea-venv/bin/ninja`），只是没进 PATH；后来直接给 glm53-venv 装了 `ninja 1.13.2`，去掉对 Talea 环境的依赖。
3. **`/usr/bin/ld: cannot find -lcudart`** → JIT 构建脚本按标准 CUDA toolkit 布局去 `-L$CUDA_HOME/lib64 -lcudart`，
   而 pip 的 `nvidia/cu13` 只有 `lib/`，且只有带版本号的 `libcudart.so.13`。修法：补 `lib64 -> lib` 软链，
   并给所有 `libX.so.N` 生成无版本号软链（真实 toolkit 里本来就有）。
4. **CCCL 拒绝编译**：`error: "CUDA compiler and CUDA toolkit headers are incompatible"`。
   根因是 pip 包混装了 **nvcc 13.2.86 与 cuda_runtime/crt 13.4.92**，
   CCCL 的 `cuda_toolkit.h` 硬性要求 nvcc 的 major.minor 与 `CUDART_VERSION` 相等。
   （原方案把 `CUDA_HOME` 指向 `talea-venv/.../nvidia/cu13`，正好踩中这个混装。）
   修法：在 glm53-venv 里装**同一发布版**的一套，全部 13.2.86。
5. **`nvvm/bin/cicc: not found`** → pip 的 `nvidia-cuda-nvcc` 不含 NVVM 组件，nvcc 调不到 `cicc`。
   修法：补 `nvidia-nvvm==13.2.86`。

修复后的 CUDA 工具链目录同时满足三件事：`bin/nvcc` 13.2、`include/cuda_runtime_api.h` 的 `CUDART_VERSION=13020`、
`lib/libnvJitLink.so.13` 存在——即一套自洽的 13.2。

### 一并做的整理

- 之前为救急在 **Talea 运行环境**（`talea-venv/.../nvidia/cu13`）里加的 `lib64` 与无版本号软链**已全部还原**，
  Talea 环境保持原样；现在所需的一切都在 glm53-venv 内自洽。
- 依赖关系上，glm53-venv 仍需要 Talea 的 site-packages 提供 `numpy` 等基础包（它是在基础解释器上建的 venv，
  并不继承 Talea 的包），因此 `PYTHONPATH` 里 **glm53 的 site-packages 必须排在 Talea 之前**。

## 4. 最终可用配置

### 4.1 容器内准备（一次性，幂等）

```bash
PY=/dev/shm/glm53-venv/bin/pip
M=--index-url; U=https://pypi.mirrors.ustc.edu.cn/simple   # 实测 49 MB/s；pypi.org 只有 2.2 MB/s

$PY install --no-cache-dir --no-deps $M $U \
  "nvidia-cuda-nvcc==13.2.86" "nvidia-cuda-runtime==13.2.86" \
  "nvidia-cuda-crt==13.2.86" "nvidia-cuda-cccl==13.2.86" \
  "nvidia-nvjitlink==13.2.86" "nvidia-nvvm==13.2.86" ninja
```

然后把 pip 的 toolkit 目录补成标准布局（`lib64` + 无版本号 `.so`）：

```bash
C=/dev/shm/glm53-venv/lib/python3.10/site-packages/nvidia/cu13
[ -e $C/lib64 ] || ln -s lib $C/lib64
for so in $C/lib/lib*.so.*; do
  link="${so%.so.*}.so"; [ -e "$link" ] || ln -s "$(basename "$so")" "$link"
done
```

### 4.2 环境脚本 `/dev/shm/glm53-env.sh`

```bash
GLM_VENV=/dev/shm/glm53-venv
GLM_SP=$GLM_VENV/lib/python3.10/site-packages
TALEA_SP=/dev/shm/talea-venv/lib/python3.10/site-packages

export CUDA_PKG="$GLM_SP/nvidia/cu13"                                  # 自洽的 CUDA 13.2
export CCCL="$GLM_SP/flashinfer/data/cccl/libcudacxx/include"          # cuda/std 只有这份
export CUDA_HOME="$CUDA_PKG" CUDA_PATH="$CUDA_PKG" CPATH="$CCCL"

export PYTHONPATH="/dev/shm/sglang-main-src/python:$GLM_SP:$TALEA_SP:/dev/shm"

_libs="$CUDA_PKG/lib"
for _d in "$GLM_SP"/nvidia/*/lib /usr/local/nvidia/lib64 /usr/local/nvidia/lib; do
  [ -d "$_d" ] && _libs="$_libs:$_d"
done
export LD_LIBRARY_PATH="$_libs${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"

export PATH="$CUDA_PKG/bin:$GLM_VENV/bin:$PATH"
export FLASHINFER_DISABLE_VERSION_CHECK=1 SGLANG_DEEPGEMM_PDL=0
```

### 4.3 启动命令

```bash
source /dev/shm/glm53-env.sh
cd /dev/shm
setsid nohup python -m sglang.launch_server \
  --model-path /dev/shm/GLM-5.3-Flash \
  --tp-size 8 \
  --host 0.0.0.0 --port 30000 \
  --kv-cache-dtype bfloat16 \
  --dsa-prefill-backend tilelang \
  --dsa-decode-backend tilelang \
  --moe-runner-backend deep_gemm \
  --disable-custom-all-reduce \
  --reasoning-parser auto \
  --tool-call-parser auto \
  > /dev/shm/glm53-server-final.log 2>&1 < /dev/null &
```

### 4.4 验证

```bash
curl -fsS http://127.0.0.1:30000/health                                  # 就绪后返回 200
curl -fsS http://127.0.0.1:30000/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"/dev/shm/GLM-5.3-Flash","messages":[{"role":"user","content":"你好，请用一句话介绍你自己。"}],"max_tokens":300}'
```

## 5. 与官方 cookbook 的对照

官方配方：<https://docs.sglang.io/cookbook/autoregressive/GLM/GLM-5.3-Flash>
（要求 SGLang ≥ v0.5.20；自带 `Verified` 的 H100/H200/B200/GB300 等命令，以及 docker 镜像 `lmsysorg/sglang:latest`。）

本次采用的正是该配方里 **HOPPER** 的那一档：

| 参数 | 官方 | 本次 |
| --- | --- | --- |
| KV cache | H100/H200 默认 BF16 | `bfloat16`（与官方一致；FP8+TRT-LLM DSA 仅 Blackwell，Hopper 上被禁用） |
| DSA backend | Hopper 用 TileLang | `tilelang/tilelang`（自动探测原本选了 `flashmla_sparse/fa3`，也属 Hopper 可用组合） |
| MoE backend | （未在 Hopper 档固定） | `deep_gemm`（`auto` 会回退到 Triton 默认配置，日志明确警告性能次优） |
| CUDA graph | 默认开 | 开 |
| TP | 官方示例 TP4 | **TP8**：FP8 权重 306 GB 在 TP4 下单卡要 ~76.5 GB，H100 80GB 太紧；TP8 后单卡 38 GB |

官方 Low Latency 档的 MTP（`--speculative-algorithm EAGLE --speculative-num-steps 5 --speculative-eagle-topk 1
--speculative-num-draft-tokens 6`）本次**不可用**，见下节。

## 6. 已知问题与未做的验证

1. **MTP 投机解码在这套组合上会崩**（本次唯一未攻克项）
   - 开 CUDA graph + MTP：生成到 512 token 附近报 `torch.AcceleratorError: CUDA error: an illegal memory access was encountered`，进程被 watchdog 收尸。短请求（64 token）能过，长生成崩。
   - 关 CUDA graph + MTP：不再崩，但吞吐掉到 **21.6 token/s**，比不开 MTP 的 113.4 差 5 倍以上。
   - 结论：当前 **不开 MTP** 是最优点。怀疑与「MTP + BF16 KV + TileLang DSA」这条未被官方实测过的组合有关，值得在升级到官方 pinned 构建后再试。
2. **TorchCodec / FFmpeg 告警**：日志里有 135 条 `libtorchcodec_core*.so` 加载失败。确认**非致命**，只影响视频输入；与交接文档的判断一致。
3. **未做的验证**：没试 EP（`--ep-size`）、没试 FP8 KV、没做并发压测、没测多模态输入、没做 GSM8K 之类的质量门。
   上面的吞吐都是**单流**数字，不能当作并发能力指标。
4. **没动仓库代码**：全部改动都在容器的 `/dev/shm` 临时环境里。模型、venv、源码、日志都会随容器重启消失。

## 7. 复现步骤（从零到服务）

1. 确认 SSH 可达（用密钥）：`ssh -i ~/.ssh/id_ed25519 -p 30027 root@120.220.102.21`
2. 按 §4.1 装齐 13.2 工具链并补软链
3. 落地 §4.2 的 env 脚本
4. 按 §4.3 启动，等日志出现 `ready to roll`（约 140 s）
5. 按 §4.4 验证

## 8. 踩坑备注（与 tai-talea 铺装设计相关）

这台机器暴露的元问题值得记进铺装清单：**「装了一堆能 import 的包」不等于「有一套能编译的工具链」**。sglang 首次前向会 JIT 编译内核，
而 JIT 需要的是**成套的**编译器 + 头文件 + 库，版本还要彼此相等。只要依赖来自多个互不匹配的 pip 包（本例 nvcc 13.2 配 CUDA 13.4 头文件），
即使 `import torch` 正常，也会在第一个请求时才炸。铺装清单的「兼容类」维度应显式包含：
nvcc 版本、`CUDART_VERSION`、CCCL 版本三者必须同源；以及 pip toolkit 需要补齐 `lib64`/无版本号 `.so`/`nvvm` 才能给 JIT 使用。
