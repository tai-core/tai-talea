# 方案 C 实测：两台 8×H100 双聚合副本 + Router

日期：2026-10-08
节点：`glm53-prefill`（c8752ad0，内网 172.19.192.134）、`glm53-decode4`（cac7f316，内网 172.19.119.194）
模型：GLM-5.3-Flash（FP8 e4m3，`/dev/shm/GLM-5.3-Flash`）

---

## 0. 结论

1. **MTP 与 CUDA graph 可以同开，已实测跑通。** 2026-09-23 那次 `illegal memory access` 是 OOM 伪装——把 `--mem-fraction-static` 从 0.70 降到 0.60 后，CUDA graph 捕获顺利完成，EAGLE 接受长度 2.65–3.65（社区报告 2.99），单流 **186 tok/s**，比 09-23 无投机时的 113.4 tok/s 高 **64%**。
2. **方案 C 有效**：两台 8 卡各跑一个完整副本，Router 前置分流，conc=16 时聚合 **2,026 tok/s**，是单流单副本的 **10.9 倍**。
3. **首次触及新 batch size 会有冷编译悬崖**（3.5s → 15s）。这是 JIT/编译开销而非容量墙，热态复跑即消失。生产必须预热或预编译。
4. 路上踩到两个阻塞项（都已修）：缺 `ninja`；ramdisk 副本缺 tokenizer 文件与 `chat_template.jinja`。

---

## 1. 部署形态

```
                     ┌──────────────────────────────┐
   client ──────────▶│ sgl-model-gateway  :30000    │
                     │ mode: Regular  round_robin   │
                     └───────┬──────────────┬───────┘
                             │              │
              ┌──────────────▼───┐   ┌──────▼───────────────┐
              │ glm53-prefill    │   │ glm53-decode4        │
              │ 8×H100  TP8/EP8  │   │ 8×H100  TP8/EP8      │
              │ EAGLE 5/1/6      │   │ EAGLE 5/1/6          │
              │ CUDA graph 开    │   │ CUDA graph 开        │
              │ :9002            │   │ :9002                │
              └──────────────────┘   └──────────────────────┘
```

两台机器人均为完整 TP8 聚合副本，Router **不带 `--pd-disaggregation`**，worker_type 为 `regular`（`prefill_count:0, decode_count:0, regular_count:2`）。

### 启动参数（两台完全一致）

```
--model-path /dev/shm/GLM-5.3-Flash
--served-model-name zai-org/GLM-5.3-Flash   # 关键：两副本必须一致
--chat-template /dev/shm/GLM-5.3-Flash/chat_template.jinja
--tp-size 8 --ep-size 8
--kv-cache-dtype bfloat16
--dsa-prefill-backend tilelang --dsa-decode-backend tilelang
--moe-runner-backend deep_gemm
--disable-custom-all-reduce
--disable-flashinfer-autotune
--speculative-algorithm EAGLE
--speculative-num-steps 5 --speculative-eagle-topk 1 --speculative-num-draft-tokens 6
--mem-fraction-static 0.60                  # 关键：0.70 会 OOM
--context-length 32768
--chunked-prefill-size 8192 --max-prefill-tokens 8192
--max-running-requests 32
--load-format runai_streamer --model-loader-extra-config '{"concurrency":16}'
--watchdog-timeout 1800 --dist-timeout 600
```

### 实测生效的关键项（启动日志核对）

| 项 | 值 |
| --- | --- |
| `speculative_algorithm` | `EAGLE`（`NEXTN` 在本版本已并入 EAGLE，枚举里**没有** NEXTN） |
| `speculative_num_steps / topk / draft_tokens` | 5 / 1 / 6 |
| `speculative_draft_model_path` | `None`（用 checkpoint 内置 MTP 头） |
| `speculative_moe_runner_backend` | `deep_gemm` |
| `disable_flashinfer_autotune` | `True` |
| `mem_fraction_static` | `0.6` |
| decode CUDA graph | `backend: full`, `max_bs: 512` |
| prefill CUDA graph | **自动禁用**（日志：`Breakable CUDA graph is incompatible with KDA hybrid linear attention`） |
| 每卡显存 | 51.6 GB（权重 37.98 GB + KV + CUDA graph） |

---

## 2. 性能实测

工作负载：39-token prompt，512-token 输出，`temperature=0`。

### 2.1 单副本基线（直连 :9002）

| 场景 | 结果 |
| --- | --- |
| 单流 TTFT | **0.15–0.19 s** |
| 单流吞吐 | **185.3–186.3 tok/s**（多次测量极稳） |
| 接受长度 | 2.65–3.65，接受率 0.33–0.53 |
| bs=4 时单副本 | ~620 tok/s（日志峰值 1,149 tok/s） |

对比 2026-09-23 同硬件无投机：113.4 tok/s → 186 tok/s，**+64%**。

### 2.2 方案 C：经 Router 的两副本

| 并发 | 聚合吞吐 | 每请求延迟 p50 | 每请求 tok/s |
| --- | --- | --- | --- |
| 1 | 186 tok/s | 2.75 s | 186 |
| 2 | **372 tok/s** | 2.75 s | 186 |
| 4 | **622–626 tok/s** | 3.07–3.27 s | 161–167 |
| 8 | **1,153–1,183 tok/s** | 3.33–3.42 s | 151–153 |
| 16 | **2,026 tok/s** | 3.87 s | 133 |

对数：conc=2 恰好 2.0×（零损耗分流）；conc=16 达单流单副本的 **10.9×**。
两副本日志峰值各 **1,149.9 / 1,160.8 tok/s**，与聚合数字自洽；两侧都出现过 `#running-req: 8`，确认分流均匀。

对比社区单节点 8×H100 实测 1,007 tok/s（HF discussion #23）——我们两台机器在 conc=8 拿到 1,153–1,183 tok/s，量级吻合。

---

## 3. 冷编译悬崖（重要）

首轮 conc=8 结果是 **272 tok/s**，看着像容量墙，实际不是：

| 现象 | 数据 |
| --- | --- |
| 首轮 conc=8 | 4 个请求 3.9 s 完成，另 4 个 15.0 s 完成 |
| 两个副本的 bs=4 批次时间戳 | prefill 14:26:14–17，decode4 14:26:25–27（**相差 11 秒**） |
| 热态重跑 conc=8 | **1,152.7 / 1,182.6 tok/s**（两轮一致） |
| 首轮 conc=16 | 522 tok/s |
| 热态重跑 conc=16 | **2,025.7 tok/s** |

根因：**某个副本第一次见到一个新的 batch size 时，要现场 JIT/编译该 shape 的 kernel**，期间引擎停摆。sglang 自己就警告过这件事：

> `Triton kernel 'chunk_kda_fwd_kernel_inter_solve_fused' took 3.05 s to compile after serving started. Serving-time compilation can stall the engine; pre-compile it during engine init.`

处置建议：
- 上线前用目标并发档位跑一轮预热（每个 batch size 至少触发一次）；
- 或按提示做 serving 前的预编译；
- 监控里把"首轮 vs 复跑"区分开，避免把冷启动误读成容量上限。

---

## 4. 本次踩到的阻塞项

| 问题 | 症状 | 修法 |
| --- | --- | --- |
| **缺 `ninja`** | `FileNotFoundError: 'ninja'`，EAGLE + tilelang 的 JIT 编译直接崩 | `apt-get install -y ninja-build`（两台的 prefill 侧缺，decode4 已有） |
| **ramdisk 副本缺 tokenizer** | warmup 报 `tokenizer.chat_template is not set`；同时 `--reasoning-parser=auto` / `--tool-call-parser=auto` 识别失败被禁用 | 从 NAS 补 `tokenizer.json` / `tokenizer_config.json` / `chat_template.jinja` / `processor_config.json`。注意 GLM-5.3-Flash 的模板在**独立的 `chat_template.jinja`**，`tokenizer_config.json` 里没有内联模板 |
| **Router 的 model_id 必须一致** | 一台带 `--served-model-name`、一台没带 → Router 视为两个模型，无法分流 | 两副本统一 `--served-model-name`；改完要**重启 Router**重新拉取 worker 信息，否则缓存旧 model_id 且 `is_healthy=false` |
| `pkill`/`pgrep` 自匹配 | SSH 单行命令里 `pgrep -f 'cp -a'` 恒真 | 把操作写成远端脚本再执行（脚本内容不出现在命令行里） |

其他观察：
- `--reasoning-parser auto` / `--tool-call-parser auto` 在 chat template 正确加载后不一定都能识别，必要时显式写 `glm45` / `glm47`。
- 权重从 ramdisk 流式加载 **7.4 GiB/s**（305.8 GiB / 41 s，runai_streamer concurrency 16）；从 NAS 拷到 ramdisk 约 400 MB/s（306 GB ≈ 13 分钟）。**重启频繁的话 ramdisk 副本值得留**。
- prefill 段 CUDA graph 被 KDA 自动禁用，decode 段保持开启——这是预期行为，不是配置错误。

---

## 5. 复现脚本

全部在 `D:/DagoPlus/.workbuddy/tmp/alayanew/`，并已推到两台机器的 `/root/`：

| 脚本 | 用途 |
| --- | --- |
| `glm-agg.sh` | 启动一个聚合副本（`MODEL=` 可覆盖模型路径） |
| `router-agg.sh` | 以聚合模式启动 Router 并注册两个副本 |
| `glm-status.sh` | 一次性状态探测（日志尾 + 显存 + health） |
| `bench.py` | 单流 TTFT / 精确实测 tok/s / 并发聚合吞吐 |
| `bench-suite.sh` | 单副本基线 + Router 全并发档位 |
| `conc-test.sh` | 高并发复测（验证冷编译假象） |

当前两个副本与 Router **仍在运行**（Router 入口 `172.19.192.134:30000`）。

---

## 6. 与方案 B（PD 分离）的关系

方案 C 换来的东西：投机解码 + CUDA graph 可用（PD 下两者都被禁），代价是少了 prefill/decode 的资源解耦。
如果后续目标转向长上下文/超大批量的吞吐，PD 仍是更合适的形态，但那意味着放弃投机解码；**两者当前不可兼得**（cookbook §3.5）。
