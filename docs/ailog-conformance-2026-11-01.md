# ai-log 与《第三方网关 Token 日志规范》v1.2 的符合性核对（2026-11-01）

对象：`kunhaoyuan` 部署里 Router 输出的 AI 访问日志。
方法：从节点 hostPath 导出**原始单行**（不经过任何展示态转换），用 `ops/check-ailog.py`
（两层契约）与 `ops/analyze_ailog.py`（规范第 8 章清单 + token 算术）逐条核对。

样本：**15,118 行 token 日志 / 137,014 行总输出 / 58.2 MB**，时间窗
`2026-11-01T01:04:03Z .. 11:07:17Z`（北京时间 09:04–19:07），来自 Router 的
`/generate`（14,786 行，压测）与 `/v1/chat/completions`（332 行）。

**分水岭**：Router 当前 Pod（镜像 `glm-5.3-flash-ailog3`）在 `08:39:55Z` 启动，
之前那 12,365 行是旧二进制写的。两侧要分开看，否则会把历史当成回归。

---

## 1. 结论

| 维度 | 结论 |
| --- | --- |
| 两层 JSON 契约 | **通过**：15,118/15,118 外层可解析，内层全部可解析，`request_id` 无重复 |
| token 口径 | **通过**：`total_token = input + output`，13,099 行零偏差；无 0 填充、无部分三元组（当前二进制） |
| 缓存字段 | **通过**：只有 `cached_tokens`，无 `cache_type`（隐式缓存正确写法），无重复加回 |
| 时间/耗时单位 | **通过**：`start_time` 全为 `...Z` 毫秒精度；耗时是毫秒（p50 6.4 s、max 56 s） |
| 当前二进制（08:39:55Z 之后 2,753 行） | **0 违规** |
| 旧二进制（08:39:55Z 之前 12,365 行） | 4 行有问题（0.03%），已随重启消失 |
| 规范第 8 章第 10、11 条（目标 Vector 实跑、隔离分支） | **未做**，需要目标集群的 Vector 配置与运行时，方法见 §6 |
| 一个真实字段缺口 | `reasoning_tokens` 被丢掉（已修，待出镜像，见 §3.1） |

一句话：**日志本身是合格的，可以交给采集端**；欠的是"在目标 Vector 上实跑一遍"这一步，
以及一个已经补掉的字段缺口。

---

## 2. 逐条核对（规范第 8 章）

| # | 清单项 | 现状 | 证据 |
| --- | --- | --- | --- |
| 1 | 每行可 `parse_json`，单行、UTF-8、无 BOM | 通过 | 15,118/15,118；文件头无 `EF BB BF`；无行内换行 |
| 2 | `ai_log` 是合法转义 JSON 字符串 | 通过 | 全部为 string，二次 `json.loads` 成功；无双重转义；无非 AI 行（Router 只记 AI 路由） |
| 3 | `input_token_details` 为嵌套对象 | 通过 | 12,990 行带 `{"cached_tokens": N}`，是对象不是字符串 |
| 4 | `request_id` 唯一 | 通过（已加固） | 15,118 个唯一值，前缀统一 `nightgw-`（形如 `nightgw-gnt-<24 随机>`）。原先**调用方自带 `x-request-id` 会被原样采用**，已在 ailog4 改为总是生成、调用方的值只记 `caller_request_id`，见 §3.0 |
| 5 | 耗时单位毫秒 | 通过 | 外层 p50 6,460 ms / p95 19,870 ms；`llm_service_duration` 同量级 |
| 6 | `start_time` 带时区、可 RFC3339 解析 | 通过 | 全部 `2026-11-01T…Z`，毫秒 3 位 |
| 7 | `gateway_type` 固定 | 通过（值待定，见 §5.2） | 全部 `nightgw` |
| 8 | `llm-response: "claude"` 仅 Anthropic | 通过 | 从未输出该字段（本部署只有 OpenAI 兼容协议） |
| 9 | OpenAI 下 `input_token` 已含命中，不重复加回 | 通过 | 所有行 `cached_tokens ≤ input_token`；无 `llm-response`，采集端不会触发加回 |
| 10 | 用目标部署的 Vector 验证完整 VRL | **未做** | 需要目标集群的 ConfigMap 与运行时，见 §6 |
| 11 | 非法外层/内层/业务过滤的行，核对采集条数 | **未做** | 需人工注入坏行，方法见 §6 |
| 12 | 中断 patch（可选） | 不适用 | Router 检测不到客户端中断，按规范"实现不了就不输出"，从未写 `ai_log_token_patch` |
| 13 | 外层 `response_code` 覆盖 200/3xx/401/502/0 | 部分 | 实到 200（13,103）、429（512）、503（1,503）。**401 永远看不到——鉴权在 nginx，被拒的请求根本到不了 Router** |
| 14 | 未知用量不补 0 | 通过 | 429/503 行完全不带三个 token 字段；`cached_tokens: 0` 是真实测量（无前缀命中），不是填充 |
| 15 | 取原始日志验证两层解析 | 通过 | 校验的是节点上的原始文件，不是平台导出或排版后的形态 |

补充核对（规范正文要求、清单未列）：

- `total_token` 缺失时由 Router 派生为 `input + output`；13,099 行全部满足，无一行偏差。
- 内层字段集合共 9 种形态，全部是规范字段的子集，**没有任何字段名带 prompt/answer/content**。
- `chat_id` 只在 `/v1/chat/completions`（含流式）出现，共 113 行，值是上游 `id`。
- `output_token_details` **一次都没出现**（原因见 §3.1）。

---

## 3. 五个真实发现

### 3.0 调用方能左右 `request_id`，重放就会撞 id（已修，11-01 20:19 上线 ailog4 验证）

Router 的 request-id 中间件默认读入站 `x-request-id` 头（`server.rs:1369` 的
`unwrap_or_else` 缺省是 `["x-request-id"]`），读到就直接用，只在没有 `nightgw-` 前缀时补一个。
实测（两条真实请求，同一个头）：

```
request_id=nightgw-dup-probe-001  start=2026-11-01T11:17:39.690Z  code=200  tokens=13/16/29
request_id=nightgw-dup-probe-001  start=2026-11-01T11:17:40.945Z  code=200  tokens=13/16/29
```

两条独立请求、同一个 id。按规范，ES 以 `requestId` 为文档 ID **幂等覆盖**，
所以第二条会盖掉第一条——**29 个 token 静默消失**，正是附录 B 里那条"request_id 重复 → 用量丢失"。

> ⚠️ 这两条探针留在日志文件里了（`11:17:39`/`11:17:40`），**下次导出的窗口里会有 1 个重复
> request_id，那是我造的，不是线上问题**。样本窗口（到 `11:07:17` 为止）里没有重复。

触发条件很普通：调用方带 `x-request-id`（做链路追踪的客户端都会带），并且**复用**了同一个值
（重试、一个会话一个 id、代理转发同一 id）。今天 15,118 行没有重复，只是因为目前的调用方没带这个头。

**已按「改代码」修掉**（`sglang-router` `feat/token-access-log`）：

- `--request-id-headers` 的缺省值由四个头改成**空列表** ⇒ Router 总是自己生成 id，
  响应头 `x-request-id` 回传的也是生成值。
- 调用方带来的值不丢，改写进**外层** `caller_request_id`（采集端不映射这个字段，所以它不参与
  ES 文档 ID，重放也不会覆盖任何人的用量）。先后顺序：`x-request-id` → `x-correlation-id`
  → `x-trace-id` → `request-id`，取第一个。
- 仍然想「调用方的 id 就是主键」的部署可以显式传 `--request-id-headers`，那时唯一性由它自己保证。
- 单测：`middleware::tests::a_reused_caller_id_does_not_become_the_request_id`（同值连发两次必须得到
  两个不同 id）、`observability::ai_access_log::tests::a_callers_own_id_is_recorded_beside_the_generated_one`；
  集成：`api_endpoints_test::request_id_tests::test_request_id_generation` 里同值重放断言 id 不同。

> 出镜像后复核：带同一个 `x-request-id` 发两次，日志里两条 `request_id` 必须不同，且两条都出现
> `caller_request_id`。核对命令见 §8。

**线上复核（Router 镜像 `glm-5.3-flash-ailog4`，20:19 滚动上去，4 台 worker 全部
`registered=4 router_healthy=4`，端点 200）**：同一个 `x-request-id: rid-probe-002` 连发两次，
两条独立记录——

```
request_id=nightgw-chatcmpl-4ZRsoMAY61ag0QL50EOJD3c5  caller_request_id=rid-probe-002  code=200
request_id=nightgw-chatcmpl-HVWkTZkNatGcI64JUg2PrOa7  caller_request_id=rid-probe-002  code=200
```

响应头也不再回显调用方的值：带 `x-request-id: rid-header-check` 请求，回来的是
`x-request-id: chatcmpl-uAulz6U5m0Ntey4Z5EJqPrns`。

### 3.1 `reasoning_tokens` 被丢掉（已修，11-01 20:19 随 ailog4 上线）

线上 `/v1/chat/completions` 的 usage 实际长这样：

```json
{"prompt_tokens":18,"completion_tokens":564,"total_tokens":582,
 "prompt_tokens_details":null,"reasoning_tokens":491}
```

`reasoning_tokens` 在 **usage 顶层**，而 Router 只找 `completion_tokens_details` /
`output_tokens_details`（Responses 协议的形态），于是这个字段从来没进过 `ai_log`，
`output_token_details` 空白。对这个 reasoning 模型，意味着**思维链和正文无法在数据上分离**：
564 个 completion 里有 491 个是思考。

影响：规范附录 A 注明 `reasoning_tokens` 当前 ES 口径不直接消费，所以**计费不受影响**；
但字段确实该有，且是唯一能看出"钱花在思考上"的地方。

已修：`src/observability/ai_access_log.rs` 在合并 usage 时把顶层的 `reasoning_tokens`
提进 `output_token_details`，并加了用例 `sglang_top_level_reasoning_tokens_reach_the_output_details`
（用上面这条真实 shape，同时断言 `prompt_tokens_details: null` 不产生假的 `input_token_details`）。
`cargo test --lib -- observability::ai_access_log`：**10 passed, 0 failed**。

**已上线**：`glm-5.3-flash-ailog4` 的第一条真实记录里就出现了
`"output_token_details":{"reasoning_tokens":24}`（24/24 个 completion token 都是思考）。

### 3.2 旧二进制留下的 4 行（不是回归，两处旧缺口）

全量里只有 4 行不合格，全部落在 `01:04–02:27Z`，即当前 Pod 启动之前：

| 行 | 现象 | 原因 |
| --- | --- | --- |
| 3 行 `/v1/chat/completions` 200 流式 | 没有 token 三元组 | 当时的二进制还没有 `--force-stream-usage`；客户端没带 `stream_options.include_usage` 就拿不到 usage。同一时段 3 条**要了** usage 的流式请求全部有值，正是这个缺口的直接证据 |
| 1 行 `/generate` 200 流式 | 有 `input_token`/`output_token`，**缺 `total_token`** | 派生逻辑（`total = input + output`）是 `f4493fa`（12:40）才加的；同一时刻的 7 条 chat 请求有 total，是因为 OpenAI 响应自带 `total_tokens`，而 `/generate` 的 `meta_info` 从来不给 |

两处都已随当前镜像消失：当前二进制 2,753 行里，流式 2,211 条（含 192 条拒绝）**零缺口**，
`total_token` 零缺失。这 4 行占旧二进制 12,365 行的 0.03%。

**它们是日志文件 append-only 的历史，不是现在的行为**——用 `start_time` 卡分水岭即可复现：

```bash
python3 ops/analyze_ailog.py /root/talea-ai-access.jsonl     # 4 行失败
# 只看当前 Pod 启动之后的：
python3 - <<'PY'
import json
cut = "2026-11-01T08:39:55"
open('/root/cur.jsonl','w').writelines(
    l for l in open('/root/talea-ai-access.jsonl', encoding='utf-8')
    if l.strip() and json.loads(l).get("start_time","") >= cut)
PY
python3 ops/analyze_ailog.py /root/cur.jsonl                  # PASS
```

### 3.3 校验脚本把"拒绝"误报成"缺用量"（已修）

`check-ailog.py` 原来把所有"流式但没有 token"的行都当异常，实测 1,258 行是这样，
**全部是 503（池子没 worker）**，其次是 429（池级限流）。规范和常识都要求这种请求
不带用量。误报的代价是真实的：它会训练人忽略这条 WARNING，从而看不见真正的 4 行。

已改为只对 **2xx 且无用量** 的流式行报警，其余计入 `refused` 单独统计。

### 3.4 `response_type` 对 429 记成 `normal`（记录在案，不改）

限流器在解析请求体之前就返回 429，Router 不知道调用方要的是不是流式，
于是按"响应不是 SSE"推断成 `normal` → 采集端得到 `isStream=false`。

只影响**被拒请求**的流式/非流式拆分；成功请求全部正确。要修得在拒绝时也解析 body，
为一个统计小项增加 429 路径上的解析开销，不值得。写在这里是为了下一个人不必再查一遍。

---

## 4. 这份日志里没有的东西（不是缺陷，是范围）

- **中断 patch**：`ai_log_token_patch` 从不输出。规范允许（"实现不了中断检测就不输出"），
  代价是客户端中途断开的请求会被记为"有用量"或"无用量"，无法用估算补。
- **Claude 协议 / 显式缓存**：本部署只有 OpenAI 兼容协议，`llm-response`、`cache_type`
  都不该出现，也确实没有。
- **`created_cache_tokens`**：sglang 不报这个扩展字段（现有四套 VRL 也不消费）。
- **业务字段**（`user_id`、`customer_id`、`agent_id`、`encrypted_api_key`…）：全部缺失。
  规范列为可选，但意味着**无法按用户/租户出账**。要按调用方分摊，得先有身份透传。
- **401**：鉴权在 nginx 完成，被拒请求到不了 Router，日志里永远没有 401。

---

## 5. 交付前需要定的两件事

### 5.1 `poolId`

规范 §7.3：`poolId` 在 Vector 配置里按集群写死，moma/vip/cluster11 = `CIDC-RP-48`，
guangzhou = `CIDC-RP-205`。kunhaoyuan 属于**独立资源池**，两个值都不对。
必须由统计侧给一个值，或为这套部署单配一份 Vector 配置——否则我们的用量会被记进别人的池子。

### 5.2 `gateway_type` 与 `request_id` 前缀

现在是 `gateway_type: "nightgw"` + `request_id: "nightgw-…"`（代码里两个常量）。
规范的本意是"B 侧固定写自有标识，用于区分流量来源"。两个选项：

- **保持 `nightgw`**：如果统计侧已经把"nightgw"当作这条链路的名字，改了就断。
- **改成 `talea`**（前缀 `talea-`）：能在一张表里区分"我们自己"和原来的 nightgw 流量。
  代价是要同步通知统计侧，且 `request_id` 前缀变化后历史记录无法按前缀聚合。

这是一行常量的决策，不是技术问题，**交给平台侧拍**。

---

## 6. 还差的两步验收（规范第 8 章第 10、11 条）

1. **在目标 Vector 上实跑 VRL**：把一行原始日志作为 `.message` 喂给
   `parse_ai_log`（文件源还要带 `.file`），确认 `promptTokens/completionTokens/totalTokens/
   implicitlyCacheTokens/cacheCreationInputTokens/inputCachePromptTokens/isStream/
   firstTokenCostTime/totalCostTime/model/gatewayType/requestId` 全部推出值。
   重点看 `request_model: "none"` 那批（`/generate` 压测流量）——它们会推出 `model=null`。
2. **注入坏行核对隔离**：分别投一条非法外层、一条非法内层，确认
   "采集条数 = 有效输出 + 丢弃 + 隔离"能对上。注意规范 §7.3.4：新增的 stdout / 文件源，
   **外层解析失败是直接丢弃，不进隔离区**，所以"隔离文件为空"不能当验收依据。

---

## 7. 日志放哪里

### 7.1 现状与它的两个问题

Router 把所有 stdout（tracing + 访问日志）一起 `tee` 到节点 hostPath
`/data02/talea-ai/logs/talea-router.log`：

| 指标 | 实测 |
| --- | --- |
| 总输出 | 137,014 行 / 58.2 MB，约 5.8 MB/h ≈ **140 MB/day** |
| 其中 token 行 | 15,118 行（**11%**），约 0.64 MB/h ≈ 15 MB/day |
| 平均行长 | 425 B |

问题一：**89% 是 tracing 噪声**，采集端每条都要过一遍正则才算得出"不是 AI 行"。
问题二（更要紧）：容器日志（stdout）由 kubelet 轮转，默认 **10 MiB × 5**。
按 5.8 MB/h 算，**容器日志只装得下最近约 1.8 小时的输出**；采集端停摆或落后超过这个窗口，
行就随轮转被删掉，账就丢了。只留 token 行的话（0.64 MB/h）窗口是 **约 16 小时**。

### 7.2 三个候选

| 方案 | 做法 | 优点 | 代价 |
| --- | --- | --- | --- |
| **A. stdout + `kubernetes_logs`**（规范 §7.1，平台推荐） | Vector 的 k8s 日志源，`extra_label_selector: "app=talea-router"` | 不依赖我们的文件；Pod 重建/漂移自动跟随；跨集群唯一物理通路 | 受容器日志轮转窗口限制（现 1.8 h）；Vector 必须跑在 router 所在节点 |
| **B. hostPath 文件源**（规范 §7.2） | Vector 挂 `/data02/talea-ai/logs`，`file` source | 与容器轮转解耦；路径可控；可回溯 | 只有 `aicube-7` 有这份文件，router 漂移就换盘；文件无限增长需 logrotate |
| **C. Router 直接写文件** | 不再 `tee` stdout，访问日志单独落盘 | 最干净：stdout 只留 tracing，文件只有 token 行 | 要改代码（小改动） |

### 7.3 建议

**先做一件与落点无关、立刻见效的事**：把 `tee` 换成只保留 token 行。

```bash
exec > >(grep --line-buffered '"ai_log"' >> "$log_file")
```

文件从 140 MB/day 降到 15 MB/day，容器日志窗口从 1.8 小时拉到 16 小时，
采集端偶尔停摆也不至于丢账。

**落点按这个顺序选：**

1. **采集端放进我们集群**（Vector DaemonSet，`nodeAffinity` 到 `aicube-7`，
   source 用 `kubernetes_logs` + label `app=talea-router`），出口指向平台的 ES / Kafka。
   这是规范的方式 A，也是唯一能跨集群拿数据的物理通路。
   **前提**：Pod 现在没有出网，得先放通到 ES / Kafka 的网络——这一步要找平台确认。
2. **若网络不放通**：Vector 落在跳板 n2，读我们导出的文件（或平台侧采集器读节点盘）。
   代价是多一条搬运链路，且 n2 到不了 Pod IP，只能读已经落盘的东西。
3. **不建议**继续把全量 stdout 写进 hostPath 当正式来源——140 MB/day 的无用噪声，
   而且它和容器日志抢同一个轮转预算，却不享受"部署漂移自动跟随"的好处。

顺带两条过滤规则，建议在采集端就做掉（不要进 ES）：

- **`/generate` 的压测流量**：占今天 98% 的行，`request_model` 是 `"none"`，
  进统计会变成 `model=null` 的一堆用量。按 `path=/generate` 或 `user_agent=python-requests` 丢弃。
- **429 / 503**：要不要进统计由平台定；进的话它们是 `callResult=false` 的失败记录，
  不进的话省得和"真实用量"混在一起。

### 7.4 落库侧

- ES：index `inference_api_tokens-YYYY-MM`，文档 ID = `requestId`（我们的 id 已全局唯一，可幂等去重）。
- Kafka：key = `requestId`（**注意规范明确写了：Kafka key 不保证消费端去重**）。
- `poolId`：见 §5.1，必须先定。
- 新增 source 后，把 source 名追加进 `parse_ai_log.inputs`（现有四套配置的 inputs 各不相同，
  以目标集群实际配置为准，不要照抄示例）。

---

## 8. 复现方式

```bash
# 跳板机上，导出最近 20000 条 token 行（只读 Pod 挂 hostPath，因为账号没有 exec/port-forward）
bash /root/talea-deploy/ops/export-ailog.sh 20000

# 规范第 8 章清单 + token 算术（严格版）
python3 /root/analyze_ailog.py /root/talea-ai-access.jsonl

# 两层契约（日常用，会给出通过/失败计数）
python3 /root/talea-deploy/ops/check-ailog.py /root/talea-ai-access.jsonl
```

两个脚本都在 `deploy/kunhaoyuan/ops/`，随仓库走。

---

## 9. 平台侧复核结果对齐（2026-11-02）

平台给了一份《流式请求计量异常分析》，结论是"成功流式请求 13 条中 4 条用量不完整
（30.77%）"。**方法没有问题，数字与本文 §3.2 完全一致**，但缺一个上下文：那份样本
（`ailog-sample-2026-11-01.jsonl`，sha256 `2473bb98…`，我们给的就是这一份）的窗口是
`01:04:03Z–04:25:24Z`，**全部早于 Router 换二进制的 `08:39:55Z`**。也就是说它测的是上版，
不是现在的状态。

### 9.1 用新样本跑同一套口径

| 口径 | 旧样本（01:04–04:25Z） | 新样本（09:06–12:19Z，ailog4） |
| --- | ---: | ---: |
| 流式 2xx | 13 | 113 |
| 三元组完整 | 9（69.2%） | **113（100%）** |
| 三个 token 全缺 | 3 | **0** |
| 有 in/out 缺 total | 1 | **0** |
| 其中 `/generate` 流式行 | 2 条，1 条缺 total | **110 条，110 条带 total** |

另有一次 1,200 行的窗口（09:00–12:19Z，含 1,160 条 `/generate`）用 `analyze_ailog.py`
跑过：`partial token triple` 0 条，唯一的失败项是本文 §3.0 里我自己造的重复 request_id。

新样本：`Documents/talea-api/ailog-sample-ailog4-2026-11-01.jsonl`（150 行）。

旧样本里这 4 条按 UA 拆开看，能直接排除"某个客户端全程拿不到用量"的可能：

| UA | 流式 2xx | 其中有 input_token |
| --- | ---: | ---: |
| `Python-urllib/3.12` | 6 | **3** |
| `curl/8.5.0` | 7 | 7 |

同一个客户端一半有一半没有 ⇒ 缺的是**那几次请求里的 `stream_options.include_usage`**，
不是网关对这个客户端做了什么。

### 9.2 四条逐条归因（与本文 §3.2 同一结论）

| 行 | 现象 | 真因 | 现状 |
| --- | --- | --- | --- |
| L2 / L106 / L107 | 200 + stream，三个 token 全缺 | 三个都来自同一个客户端（UA `Python-urllib/3.12`），请求里**没带** `stream_options.include_usage`；sglang 不主动在流里返回 usage，旧 Router 也不代填。**同一 UA 的 6 条流式里另外 3 条就有 usage** ⇒ 差别在这一次请求带没带参数，不在网关 | 已修：Router 加 `--force-stream-usage`，对 `/v1/chat/completions`、`/v1/completions` 这两条接受 `stream_options` 的路由代填（`router.rs:523` / `890`），客户端不用改——代价是多收到一帧用量分片，见 §10.4 |
| L114 | `/generate` 流式，1/4 但缺 total | 当时 total 完全取决于上游那次有没有给；**同接口同数字的 L119 就有 total**（`curl/8.5.0` 两条），说明不是接口固定行为，而是"给了就抄、不给就没有" | 已修（`f4493fa`）：`total_token` 缺失时用 `input+output` 推导，`ai_access_log.rs:565` 有注释和用例 `meta_info_without_a_total_still_writes_one` |

所以平台在 §5.1 列出的四种可能里，**是第 1/2 种（请求侧未开启 usage），不是"网关没消费"
也不是"日志写早了"**——否则同一客户端的另外几条不会有 usage。

### 9.3 要回给采集端的两条

1. **缺失 token 输出成 0（他们 §5.3 自己发现的）违反规范**。规范 §4.1 的 `total_token`
   条目写明"可省略三个 token 字段，采集结果为 null，**不得把未知用量填成 0**"，§4.5
   "值与缺省约定"又写"数值缺省 → 直接省略字段"。现在 0 让"未知"和"真实零消耗"在 ES 里
   无法区分，而且 `callResult=true` 会把它标成成功。建议 VRL 改为不产出这三列（或产出 null），
   由下游决定怎么呈现。**这条不在我们这边。**
2. **`request_model: "none"` → `model=null`**（L114/L119，即 `/generate` 流量）。
   `/generate` 请求体里没有 model 字段，是压测接口。要不要用 worker 的模型路径代填，
   需要平台定口径——本文 §5.2 已列为待定项。

### 9.4 待复核（当前登录不了机器）

以上"新样本"结论只用本地已导出的两个文件验证过；等能登录后应再做一次：
`export-ailog.sh 5000` → `analyze_ailog.py`，确认 `partial token triple = 0` 且
`/generate` 流式全部带 `total_token`。

---

## 10. 用模拟后端重放这几种情况（2026-11-02）

没有 GPU 也能验证那两处修复：`llm-d-inference-sim`（Go 写的假后端）当 worker，11-01 那个
release 二进制当 Router，两者都跑在跳板机上，不碰集群。脚本
`deploy/kunhaoyuan/ops/sim-stream-check.sh`，四组断言八条检查，最后一次运行全部通过。

### 10.1 为什么这个 sim 能当替身

它决定要不要在流里给 usage 的条件是
`sendUsageData = true; if IsStream() { sendUsageData = IncludeUsage() }`
（`pkg/endpoint/request.go:265`）——**流式、且请求没要 usage，就一帧都不给**。这与本文对
L2/L106/L107 推断的上游行为是同一件事，所以在它上面能不能复现，本身就是对归因的检验。

替代范围必须说清楚：只对 `/v1/chat/completions` 成立。它的 `/generate` 只由 vllm 引擎注册、
路径是 `/inference/v1/generate`，返回的是 `usage` 对象而不是 sglang 的 `meta_info`
（`pkg/api/response.go:715`），**不能**拿来验证 L114 那条路。L114 的形态用 §10.3 的办法单独造。

### 10.2 复现出来的就是线上那三条

| 组 | Router 参数 | 客户端请求 | ai_log 里的 token | 客户端拿到的 usage |
| --- | --- | --- | --- | --- |
| 1 | 无 `--force-stream-usage` | 流式，没要 usage | **三个都没有** | `"usage":null` |
| 2 | 无 | 流式，`include_usage:true` | 8 / 2 / 10 | 有 |
| 3 | `--force-stream-usage` | 流式，没要 usage | 8 / 4 / 12 | 有 |

第 1 组写出的 ai_log 逐字如下：

```json
{"gateway_type":"nightgw","request_model":"testmodel","response_type":"stream","llm_first_token_duration":1,"llm_service_duration":1}
```

`response_type` 是 `stream`、`response_code` 是 200、三个 token 一个没有，与线上 L2/L106/L107
完全同形。也就是说那个 30.77% 不需要 GPU 就能重放，不是"只有在集群里才出现"的现象。

### 10.3 total 兜底：单独造一个缺陷上游

sim 永远会把 total 填上，所以另起一个**只删 `total_tokens`** 的透传代理
（`deploy/kunhaoyuan/ops/strip_total.py`），造出 L114 那种"给了 in/out 没有 total"的形态：

- 客户端收到 `{"prompt_tokens":8,"completion_tokens":3,"prompt_tokens_details":{...}}`，**没有 total**
- Router 的 ai_log 写 `input_token:8, output_token:3, total_token:11`
- 客户端收到的那个对象一个字节没变 ⇒ **兜底只修日志，不改响应**

### 10.4 顺带被实测确认的一个对外行为

`force_include_usage`（`router.rs:905`）是直接覆盖，不是"仅在缺失时补"：客户端写
`include_usage:false` 也会被改成 `true`。所以开着 `--force-stream-usage` 时，**没要 usage 的
客户端也会在流末尾多收到一帧 `choices: []` + `usage` 的分片**——第 3 组比第 1 组多的就是这一帧。

`deploy/kunhaoyuan/README.md` 早已记过这条，现在有实测支撑。但交接文档 §5 之前反着写
（"不要 usage 就没有用量"），已一并修正：按 `chunk.choices[0].delta` 取值的客户端要防空
`choices`。

### 10.5 复跑

```bash
# 跳板机上，两个二进制就位后
SIM_BIN=/root/simtest/sim-linux \
ROUTER_BIN=/root/builds/sglang-router-target/release/sgl-model-gateway \
./sim-stream-check.sh
```

sim 的随机长度每次不同，所以脚只比较"有没有 token / 有没有 usage 对象"，不比较分片数——
同一份请求体连发两次得到的分片数都不一样，按分片数断言会假红。
