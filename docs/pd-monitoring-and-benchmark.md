# P/D 监控与控制节点压测

Talea 自己持续采样，浏览器只读取快照。采集、角色校验、注册修复和 benchmark 不依赖 AI Agent。

## GPU 规格与 P/D 角色

SSH 上线流程在目标容器执行 `nvidia-smi --query-gpu=name --format=csv,noheader`，将 GPU 名称和可见数量写入节点规格。合作商通过 Push 上报的规格则来自 Push payload。因此节点表里的 GPU 列是容器规格，不是实时 GPU 利用率，也不是模型实际使用的卡数。当前探测按同型号 GPU 的容器设计，使用第一张卡的型号作为名称。

当前测试环境每个容器可见 4 张 H100，但运行环境设置 `CUDA_VISIBLE_DEVICES=0`，每个服务 `TP=1, DP=1`，实际各使用 1 张卡。负载表另外显示从 SGLang `/server_info` 读取的 TP/DP。

角色由 PD Planner 分配。Bootstrap 使用 `--disaggregation-mode=prefill/decode` 启动服务，然后 Talea 按同一角色注册 Router。采样器核对三个来源：数据库分配、SGLang 实际模式、Router Worker 类型/地址。任一不一致，指标标记不可用，不会当成正常均衡。

采样器还验证 Router 的健康、readiness 和 `__pd_state`。健康失败、路由关闭或 PD 池仍在摘流时，该节点不计作正常容量。readiness 为 ready 本身不足以保证 Router 的其他门控也允许请求。

修复了重复上线/角色切换时复用旧 Router 角色的问题。正常摘流仍然只关闭 readiness；异常角色恢复先关闭 readiness、确认负载为零，再清理错误 membership，并等待新注册的地址和角色可见后才开放路由。未知或非零负载会阻止清理。注册失败但模型仍健康时，Talea 修复注册，避免重复启动同一模型。此清理符合开发文档 §8 的“最终清理或异常恢复”，不代替正常 drain。

bootstrap 返回结构化 `HTTP 503 + phase=FAILED` 时，Talea 区分“模型进程失败”和“容器不可达”，保留容器状态、关闭路由并按启动重试预算恢复。未知格式的 503、鉴权失败或连接失败仍按不可达处理。

## 实时看板

接口：`GET /v1/capacity/console/telemetry`，使用管理令牌或合作商页面令牌。合作商的当前值、聚合值、历史曲线都只包含自己的节点。

Talea 每 3 秒采样 SGLang `/metrics`、`/server_info` 和 Router，最多并发采集 8 个节点；网络调用有独立超时。页面每 3 秒刷新，控制节点内存保存最近 100 次（正常约 5 分钟）采样。控制进程重启后重新累计。生命周期控制不等待采样。

页面使用 P、D 两张吞吐卡片，不显示 P−D 压力差。负载图使用独立图例、实际时间刻度与渐变填充，支持悬停或聚焦后使用左右方向键查看采样值；按容器尺寸和屏幕像素比绘制，避免文字拉伸。缺失采样或超过 15 秒的采样间隔断开曲线，不插值为零。

| 指标 | 口径 |
|---|---|
| P 输入 token/s | P 上 `prompt_tokens_total` 相邻采样增量 / 时间；包含前缀缓存命中 |
| D 输出 token/s | D 上 `generation_tokens_total` 相邻采样增量 / 时间 |
| 运行请求 | `num_running_reqs` |
| 等待请求 | 调度队列 `num_queue_reqs` + 本角色预分配队列 |
| KV 在途 | P 的 prefill inflight 或 D 的 decode transfer 队列，单独显示 |
| 并发上限 | `/server_info` 的 `effective_max_running_requests_per_dp` 实际值 |
| 并发压力 | `(运行 + 等待) / 并发上限`；聚合时先求和再相除，可超过 100% |
| KV 缓存占用 | `token_usage`；多 DP rank 显示最高占用 |
| Router 负载 | `/get_loads` 返回的 worker load；不作为调度队列长度 |

请求/队列的 priority 汇总只使用 `priority=""`，避免将总计与各优先级重复相加；SGLang 默认由每个 DP 的单一 TP rank 发布调度指标。当前验收覆盖 TP=1/DP=1；复杂 PP、开启全 scheduler 指标的部署应先验证聚合口径。

计数器重置、初次采样、超过 20 秒的速率窗口都显示未知。采样超过 15 秒不再用于当前压力聚合；一个角色有任一节点缺少指标时，不将部分节点之和显示成全部。SGLang 调度 gauge 由服务内部更新，空闲时可能约 30 秒才刷新一次，因此成功抓取时间不等于每个 gauge 的计算时间。短请求也可能落在 3 秒采样间隙，不能据此判断从未繁忙。

`--enable-metrics` 必须在服务启动时启用。Bootstrap 已支持该允许参数，测试控制节点启动配置和 SSH 分发源已更新。仅更新配置不会修改已有进程，需通过 Talea 完成摘流和重启。

## Planner 的使用边界

每轮 `Rebalance` 将完整、未过期的调度队列、P/D 压力、压力差和两侧 token 速率送入 `planner.LoadSnapshot`，结果包含 `observed_load`。管理接口 `GET /v1/capacity/planner` 提供 `last_observed_load`，可以核对上一轮输入及其时间。

当前仍是固定配比 Planner，不自动修改档位。看板的提示是瞬时诊断，不能直接视为扩缩容命令。只有两台工作节点时需保留 1P1D，没有可用于改变 P:D 比例的额外节点。后续自适应策略应结合持续时间窗、请求输入/输出长度、TTFT/TPOT、传输排队、角色内节点差异，并沿用最小持有时间、冷却和每轮变更上限；不能直接比较 P 输入 token/s 与 D 输出 token/s 大小。

## 在控制节点运行 benchmark

```sh
talea benchmark --requests 64 --concurrency 4 --max-tokens 256
```

此命令在执行它的机器产生负载，请在控制节点运行。客户端 `/etc/tai-talea/client.json` 已配置 `router_url` 和 `model`；其他环境可显式指定 `--base-url`、`--model`。安装时将 `tools/talea_benchmark.py` 与 `tools/talea.py` 放在同一目录。

首次运行在控制节点下载官方 GSM8K test 数据集，之后复用缓存并校验固定 SHA256：

- 仓库：[openai/grade-school-math](https://github.com/openai/grade-school-math)
- 固定提交：`3101c7d5072418e28b9008a6636bde82a006892c`
- 数据：`grade_school_math/data/test.jsonl`，1,319 条，MIT 许可
- SHA256：`3730d312f6e3440559ace48831e51066acaca737f6eabec99bccb9e4b3c39d14`

命令以固定随机种子打乱问题，默认先预热 2 条，预热不计入结果。请求经过控制节点 Router `/v1/chat/completions`，使用 streaming 和 usage。报告只保留统计和每请求时延/实际 token 数，不保存问题或回答文本。数据集的标准答案不参与评分，本工具测服务性能，不测模型答题正确率。

可选 `--ignore-eos` 用于固定输出长度压测；此时负载特征不同，报告会记录。并发范围 1–64，请求数量 1–10000，输出上限 4096 token，超时上限 300 秒。每个输出目录仅允许一个 benchmark，Ctrl+C 停止提交新请求，在途请求受超时约束。强制杀进程可能留下 `active.lock`，确认其中 PID 已退出后可删除该锁。

报告目录：`/opt/tai-talea/benchmarks/`。每次产生唯一的 `gsm8k-<UTC时间>.json`，包括数据集版本/校验值、参数、成功/失败数、完成请求/s、输入/输出 token/s、TTFT 和总延迟的均值/p50/p95，以及每请求平均 TPOT 的分位数。TPOT 使用 `(最后内容到达时间−首内容到达时间)/(输出 token 数−1)`，不是逐 SSE chunk 或逐 token 抖动的 p95。首个 role-only chunk 不计为首 token；缺少 usage、流截断、请求超时均计失败。吞吐分母包括失败请求耗费的时间，分子只统计成功请求。

正式容量规划应使用接近实际业务的输入/输出长度、持续时间和缓存命中率。短 GSM8K 测试可验证吞吐及排队链路，不等于集群最大容量；比较并发时还需考虑前一轮留下的前缀缓存。

## 当前 SGLang 运行时兼容修复

真实并发测试发现 `0.0.0+g5a26fc1f.cu12` 的 `ScheduleBatch.prepare_for_decode()` 在批次过滤/合并使 `seq_lens_sum=None` 后仍执行 `+= bs`，导致 Decode scheduler 崩溃。`provision/sglang_compat.py` 对固定源码 SHA256 `67bd70ee2c2a4e7f2d9ac7a59ccd5e8a8f71bd2cb8c97ad33d58ec2a5dc05218` 应用小范围修复：缓存为空时从已更新的 CPU seq_lens 求和；已有缓存继续增量更新。

SSH 安装器在 wheel 安装和 pip check 后应用此修复，保留原文件备份，验证重复应用的来源。该版本出现其他未识别修改时拒绝覆盖。其他 SGLang 版本不应用此补丁；升级运行时应重新进行并发验收。离线 wheel 本身的校验值保持原值，安装后的本地兼容修复另行明确记录。

## 2026-09-18 实测结果

请求由控制节点发起，经同机 Router 分发到 talea1（P）与 talea0（D）。模型 Qwen2.5-3B-Instruct；两侧各使用 1 张 H100 80GB，TP=1/DP=1、最大运行并发 8、max-total-tokens=8192，NIXL/UCX TCP。GSM8K 同一随机种子，每轮 64 条、预热 2 条、输出上限 256、temperature=0，正常 EOS 提前结束。

| 并发 | 成功 / 失败 | 耗时 | 请求/s | 输入 token/s | 输出 token/s | TTFT p50 / p95 | 每请求平均 TPOT p95 |
|---|---|---|---|---|---|---|---|
| 1 | 64 / 0 | 52.26 s | 1.225 | 110.50 | 281.60 | 35.95 / 49.50 ms | 3.42 ms |
| 8 | 64 / 0 | 7.47 s | 8.563 | 772.72 | 1,951.13 | 45.27 / 121.70 ms | 4.43 ms |

两轮输入均为 5,775 token；输出分别为 14,717、14,582 token。并发 8 的输出吞吐约为并发 1 的 6.93 倍，但首 token 延迟有所增加。这是小模型、短输入、短时间测试的观察值，不是最大生产容量。两轮顺序执行，第二轮可能受前缀缓存影响；正常 EOS 和批处理也使实际输出长度不同。

看板采样捕获到 D 的运行请求从 1 提高到 7，并发压力从 12.5% 提高到 87.5%，D 的 3 秒窗口峰值约 2,106 token/s。两轮采样均完整；没有观察到持续队列堆积。P 的运行瞬间短于采样间隔，本次采样为 0，不能推断 P 没有计算开销。P 的 KV 在途 gauge 在测试前后持续为 1，而 Router load 与业务运行/队列均为 0；保留上游原始值，不擅自扣除，不能单独据此决定扩容或认定业务请求卡住。SGLang 健康探测也会产生特殊生成请求，后续自适应验收应进一步区分后台探测与业务负载。

最终报告在控制节点：

- `/opt/tai-talea/benchmarks/gsm8k-20260918T140151.051430Z.json`
- `/opt/tai-talea/benchmarks/gsm8k-20260918T140246.995216Z.json`
- `/opt/tai-talea/benchmarks/telemetry-20260918T140149Z.jsonl`

本地副本及最终 API 状态位于 `D:\DagoPlus\.workbuddy\tmp\three-node-20260918\evidence\pd-benchmark`。最终 Talea SHA256 为 `b9472dab87a73d1f83029355f0f95391f164133f73e3bedb729448551e3cc039`；两节点均 SERVING、无回收意图，Router 角色/健康一致，Planner `last_observed_load.valid=true`，压测进程已结束。

验收前一次未修复运行时的并发 8 测试发生调度器崩溃，失败报告保留，不计入上述结果。修复通过 Talea SSH 上线流程自动安装。崩溃后的 Router 遗留摘流状态本次在工作节点重启且 Router load 为零后通过维护重启清理，Talea 自动重新注册；此类 Router 异常门控的自动恢复尚未实现，不能声称所有运行时故障均已实现无人干预恢复。监控已将该状态识别为不可用，而不是正常均衡。

验证：Go 全量 `test`/`vet`、bootstrap 55 项、CLI/benchmark 7 项、provision 4 项（Windows 跳过 1 项 Linux 文件锁用例）、页面 JavaScript 语法检查、真实两轮 1P1D benchmark 通过。页面接口及部署资源已核验；本轮未完成浏览器完整视觉验收。
