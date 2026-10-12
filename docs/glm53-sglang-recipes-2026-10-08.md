# GLM-5.3-Flash × SGLang 社区配方调研

日期：2026-10-08
目的：为「两张 8×H100 跑 GLM-5.3-Flash」确定最佳配置组合，并核实 MTP 与 CUDA graph 能否同开。

---

## 0. 结论速览

| 问题 | 结论 |
| --- | --- |
| MTP 与 CUDA graph 能否同开？ | **能**，且是官方 Low Latency 配方的默认组合（单机聚合模式） |
| 我们 09-23 的 `illegal memory access` 是不兼容吗？ | **不是**。那是 OOM 伪装（官方 FAQ 明说 OOM 常报成 illegal memory access）。HF 实测同一现象：`mem-fraction-static 0.70` 时 EAGLE draft-worker CUDA graph state init OOM，降到 **0.60** 即可 |
| 2×8 卡 PD 分离下能开 MTP 吗？ | **不能**。官方 cookbook §3.5 明确：投机解码在 PD 下起不来（prefill 角色 draft-graph capture width assert；decode 侧 TP2 显存压力），要求 **PD 时不带任何 `--speculative-*`** |
| 那「两张 8 卡 + MTP + CUDA graph」怎么同时满足？ | 走**两个独立聚合副本 + Router**，而不是 PD 分离。见 §5 方案 C |

**一句话**：你的判断对了一半 —— MTP 和 CUDA graph 确实能同开，但前提是**单机聚合**；一旦拆成 PD 1P1D，投机解码在当前版本上直接被禁用，两者互斥。

---

## 1. 官方 cookbook 要点

来源：https://cookbook.sglang.io/autoregressive/GLM/GLM-5.3-Flash

### 1.1 版本与镜像

- **需要 SGLang v0.5.20 或更新**（GLM-5.3-Flash / `Glm5NextForConditionalGeneration` 支持）
- 官方镜像 `lmsysorg/sglang:glm-5.3-flash`（**CUDA 13**）；`docker pull lmsysorg/sglang:latest` 亦可
- 注意：容器内没有 docker，我们只能走 venv + 源码安装，镜像不可达（沿用 09-23 的路线）
- 我们 09-23 用的上游源码已含 `sglang/srt/models/glm5_next.py` 与 `speculative/dflash_utils.py`，即已满足 v0.5.20 门槛

### 1.2 两档策略

- **Low Latency**：MTP 5/1/6 投机解码 + 张量并行，面向交互式 / agent 负载
- **High Throughput**：投机解码**关闭**，避免持续批处理下的 draft-and-verify 开销

### 1.3 关键参数语义

- **`--speculative-algorithm EAGLE`**：新版把旧的 `NEXTN` 拼写折叠进 `EAGLE`，官方生成命令统一用 EAGLE。z.ai 的旧示例仍写 `NEXTN`。**两者是同一个 MTP 头**，不要再当作两套方案
- **`--speculative-adaptive`**：运行时按批大小动态调 `num_steps`/`draft_tokens`。**仅支持 `EAGLE`/`EAGLE3` 且 `--speculative-eagle-topk 1`**，否则静默回退到静态设置。默认候选档 `[1, 3, 7]`
- **双内存池**：KV pool（注意力）+ KDA state pool（线性注意力）。KDA 池可能先于 KV 池卡住 `max_running_requests`
  - 调 `--mamba-full-memory-ratio`（默认 0.9 是通用起点、非调优值）或钉 `--max-mamba-cache-size`
  - **不要**通过 `--json-model-override-args` 覆盖 `linear_lower_bound`
- **KV / DSA 必须配套**：
  - H100 / H200 → **BF16 KV + TileLang DSA**（FP8 KV + TRT-LLM DSA 在 Hopper 上被禁用）
  - Blackwell → FP8 KV + TRT-LLM DSA（GB300 实测吞吐 +2.9~5.7%，KV 容量 ×1.8）
  - **TileLang DSA + FP8 KV 在 CUDA 上是非法组合**
- **Breakable CUDA Graph**：`--cuda-graph-backend-prefill breakable`，需含 PR #38522 的构建，默认关闭
- **DFlash2**：用训练好的 block-diffusion draft 替换 checkpoint 内置 MTP 头，draft 跑在 `fa4` 上。权重仓库 `incoai/GLM-5.3-Flash-DFlash2` 是 **access-gated**，需申请

### 1.4 PD 分离的限制（§3.5，直接影响我们的 1P1D 方案）

原文要点：

> Speculative decoding does not start under PD at the current cut (draft-graph capture width assert on the prefill role; decode-side memory pressure at TP2). **Run PD without speculative flags.**
>
> Prefill and decode with different TP sizes transfer state through slice paths, but numeric correctness is unverified. **Keep both roles at the same TP size.**

推论：
1. PD 与投机解码**当前互斥**；
2. PD 两侧 **TP 必须相同**（我们之前设想的 P/D 不同 TP 不成立）；
3. 该限制是 "at the current cut"，属版本相关，未来可能放开 —— 但不应作为近期方案前提。

---

## 2. 社区实测：8×H100 最佳配置

来源：https://huggingface.co/zai-org/GLM-5.3-Flash/discussions/23 （Vertex AI a3-highgpu-8g，8×H100 80GB）

### 2.1 可用配方

```
--tp-size 8 --ep-size 8
--kv-cache-dtype bfloat16            # H100 必须 BF16
--dsa-prefill-backend tilelang
--dsa-decode-backend tilelang
--moe-runner-backend deep_gemm
--speculative-algorithm EAGLE --speculative-num-steps 5 \
--speculative-eagle-topk 1 --speculative-num-draft-tokens 6   # MTP 5/1/6
--disable-flashinfer-autotune        # 关键，社区点名的必需项
--mem-fraction-static 0.60           # 关键：cookbook 的 0.70 会 OOM
```

CUDA graph **保持开启**（不加 `--disable-cuda-graph`）。

### 2.2 实测数字

| 指标 | 数值 |
| --- | --- |
| 持续输出吞吐 | 1,007 output tok/s（开 reasoning） |
| 服务端报告吞吐 | 1,445 tok/s |
| EAGLE 平均接受长度 | **2.99**（约 3× 解码加速） |
| KV cache 容量 | 285,952 tokens |
| 每卡显存构成 | 38 GB 权重 + 3.4 GB KV + **13.5 GB CUDA graphs** |
| 长上下文 | 90K token prompt 无退化、无循环 |
| 成功率 | 423 请求 100%（并发 1–32 扫描 + conc 8 持续 60s） |

### 2.3 两个坑（我们要判断是否命中）

1. **`--mem-fraction-static` 必须从 0.70 降到 0.60**
   > the EAGLE draft-worker CUDA graph state init OOMs at 0.70 on 80GB H100s. 0.60 leaves enough headroom.
   注意 13.5 GB 的 CUDA graph 显存占用 —— 这正是我们 09-23 撞墙的地方。
2. **旧驱动（535 / CUDA 12.2）会导致 deep_gemm 崩**
   deep_gemm 0.1.7 的 JIT loader 需要 `cuLibraryLoadData` 等 5 个 driver ≥550（CUDA 12.4）的 API。driver 535 上 dlsym 全 NULL，服务在 CUDA graph capture 阶段以 `Failed to load CUDA driver API` 死掉。
   **我们的 8×H100 机器是 driver 580.173.02，天然免疫，不需要社区那个 shim。**

### 2.4 另一份国内 Hopper 8 卡生产基线（对照）

来源：https://blog.csdn.net/qq_35209305/article/details/164295828

```
--tp-size 8 --ep-size 8 --mem-fraction-static 0.75
--context-length 1048576 --max-total-tokens 1048576
--max-running-requests 32 --chunked-prefill-size 16384 --max-prefill-tokens 16384
--kv-cache-dtype bfloat16
--dsa-prefill-backend tilelang --dsa-decode-backend tilelang
--moe-runner-backend deep_gemm --disable-shared-experts-fusion
--disable-cuda-graph
--speculative-algorithm EAGLE --speculative-num-steps 5 --speculative-eagle-topk 1 \
--speculative-num-draft-tokens 6 --speculative-adaptive
```

差异点值得注意：这份基线**主动关掉 CUDA graph**，因为要撑 1M 上下文（KV 吃满显存）。这印证「开 CUDA graph 与开长上下文抢显存」，二者需要按目标工作负载取舍。

### 2.5 其他社区信息

- **自适应投机**：`--speculative-adaptive` 在批大小波动时有用，但只支持 topk=1。（https://sgl-project.github.io/advanced_features/adaptive_speculative_decoding.html）
- **NextN 加载缺陷**：上游按子串把 `model.layers.` 改写成 `model.decoder`，导致 GLM-5.3 的 `model.language_model.layers.45.*` 变成 `...decoder.*`，draft 模块绑不上。**影响所有 GLM-5.3-Flash 转换版本**，需要带修复的构建。（https://huggingface.co/tattrongvu/GLM-5.3-Flash-NVFP4）
- **HiCache 与 EAGLE 不能共存**（社区原话 "eagle cannot cohabit"）—— 若后续想上 HiCache，得先关投机
- **PD + 投机**：Dynamo 侧（SGLang backend）把「Disaggregated P/D speculation」标为未验证/未文档化，EAGLE state-transfer 路径存在但无 E2E 覆盖。（https://github.com/ai-dynamo/dynamo/issues/15250）
- **CUDA graph 批大小与显存**：官方调优文档明确「CUDA graph 吃更多显存，需同步降低 `--mem-fraction-static`」；`--cuda-graph-max-bs-decode` 可用来压 graph 显存。（https://sgl-project.github.io/advanced_features/hyperparameter_tuning.html）

---

## 3. 修正我们 09-23 的结论

`docs/glm53-h100-validation-2026-09-23.md` 当时的结论是：

> 官方 Low Latency 档的 MTP 本次**不可用**。开图会 `illegal memory access` 崩溃，关图只有 21.6 tok/s（比 113.4 差 5 倍）。

**该结论需要作废。** 依据：

1. 官方 FAQ：OOM 常常**不报 "Out of Memory"**，而是报 `CUDA error: an illegal memory access was encountered`。我们当时的报错形态与之完全一致。
2. 社区在**同一硬件（8×H100 80GB）**上复现了同一现象，且给出确定性根因：**EAGLE draft-worker 的 CUDA graph state 初始化在 `mem-fraction-static 0.70` 时 OOM**。
3. 修法极简：降到 `0.60`，并加 `--disable-flashinfer-autotune`。社区随后跑出 accept length 2.99 / 1007 tok/s。

所以 09-23 的 "MTP 不可用" 实际是 "MTP + 默认显存参数不可用"，不是能力缺失。**这正是本次要重新验证的第一件事。**

---

## 4. 待验证清单（转成实验项）

1. **单机 8 卡复跑 MTP + CUDA graph**，用 `--mem-fraction-static 0.60 --disable-flashinfer-autotune`
   - 判据：能进 CUDA graph capture 完成、能出 token、accept length > 2
   - 若仍 OOM：逐步下探 0.55/0.50，并压 `--cuda-graph-max-bs-decode`
2. **确认 `--speculative-algorithm` 取值**：新构建用 `EAGLE`，旧构建用 `NEXTN` —— 以实际构建的 `--help` 为准
3. **实测 PD 下投机是否真的起不来**（cookbook 说不行，但我们跑的是内部 fork，值得花一次实验确认，并把报错原文记下来）
4. **测 `--speculative-adaptive`** 对批大小波动负载的收益
5. 确认 shim 无关性：核对容器 `nvidia-smi` driver ≥ 550

---

## 5. 方案选项（两张 8×H100 的三种用法）

| 方案 | 布局 | 投机解码 | CUDA graph | 吞吐取向 | 成熟度 |
| --- | --- | --- | --- | --- | --- |
| **A** | 单机聚合 8 卡 | ✅ 5/1/6 | ✅ | 低延迟 / 交互 | 社区已验证（1007 tok/s） |
| **B** | PD 分离 1P1D（P 8卡 + D 8卡） | ❌ 官方禁 | ❌（同禁） | 高吞吐、长上下文 | 我们 09-30 已跑通（无投机） |
| **C** | 两台 8 卡各做**独立聚合副本** + Router 在前 | ✅ 5/1/6 | ✅ | 低延迟 **且**水平扩展 | 需要 Router 侧 DP，已有 sglang-router |

**方案 C 是「既要两张 8 卡、又要 MTP + CUDA graph」的唯一解**：放弃 PD 拆分，把两台机器当作两个完整副本，由 Router 做前向分流。代价是少了 PD 的 prefill/decode 资源解耦；收益是投机解码与 CUDA graph 都能开。

如果目标确实是 **长上下文 + 高吞吐**（1M 上下文、大批量），PD 方案的收益更大，此时应按 B 执行，并接受关掉投机与 graph。

---

## 6. 落到 Talea 的改动面

`bootstrap/tai_talea_bootstrap/roles.py` 的 `ALLOWED_EXTRA_FLAGS` 目前**不含**新配方需要的参数，直接下发会被 400 拒绝：

缺失项（按用途分组）：

- 投机：`--speculative-algorithm`、`--speculative-num-steps`、`--speculative-eagle-topk`、`--speculative-num-draft-tokens`、`--speculative-adaptive`、`--speculative-attention-mode`、`--speculative-moe-a2a-backend`、`--speculative-accept-threshold-single`、`--speculative-accept-threshold-acc`
- CUDA graph：`--cuda-graph-max-bs-decode`、`--cuda-graph-backend-prefill`、`--disable-cuda-graph`
- DSA/量化：`--quantization`、`--page-size`、`--linear-attn-verify-backend`
- 调度/显存：`--ep-size`、`--context-length`、`--max-prefill-tokens`、`--mamba-full-memory-ratio`、`--max-mamba-cache-size`、`--min-free-slots-delay`、`--prefill-decode-interval`
- 其他：`--disable-flashinfer-autotune`、`--disable-shared-experts-fusion`、`--mm-feature-transport`、`--watchdog-timeout`、`--dist-timeout`

> 白名单是刻意收窄的防线（M1.md §4），扩容需逐项确认「控制面确实会下发、且不构成逃逸面」。

---

## 7. 来源

- SGLang Cookbook — GLM-5.3-Flash：https://cookbook.sglang.io/autoregressive/GLM/GLM-5.3-Flash
- HF 实测讨论（8×H100）：https://huggingface.co/zai-org/GLM-5.3-Flash/discussions/23
- SGLang FAQ（illegal memory access ≈ OOM）：https://sgl-project.github.io/references/faq.html
- SGLang 自适应投机解码：https://sgl-project.github.io/advanced_features/adaptive_speculative_decoding.html
- SGLang 超参调优（cuda-graph-max-bs-decode / mem-fraction-static）：https://sgl-project.github.io/advanced_features/hyperparameter_tuning.html
- 国内 Hopper 8 卡生产基线：https://blog.csdn.net/qq_35209305/article/details/164295828
- GLM-5.3-Flash NVFP4 服务说明（NextN 加载缺陷）：https://huggingface.co/tattrongvu/GLM-5.3-Flash-NVFP4
- Dynamo：SGLang 后端投机解码支持现状：https://github.com/ai-dynamo/dynamo/issues/15250
- GLM-5.3-Flash NVFP4 支持 PR（EAGLE 启动配置参考）：https://github.com/sgl-project/sglang/pull/38621
