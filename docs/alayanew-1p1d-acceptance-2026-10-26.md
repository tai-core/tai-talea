# 九章智算云：GLM-5.3-Flash 1P1D 端到端验收（2026-10-26）

## 0. 结论

**tai-talea 在九章 CCI 上把 GLM-5.3-Flash 跑成了 1 prefill + 1 decode，并经 Router
完成了真实的 PD 分离推理。**

| 项 | 结果 |
| --- | --- |
| 受管实例 | `glm53-pd2`（prefill）、`glm53-pd3`（decode），两台都 `SERVING` |
| Router | `prefill_count=1`、`decode_count=1`，两台 `is_healthy=true` |
| 真实请求 | `POST /v1/chat/completions` → 200，101 prompt tokens + 64 decode tokens，**0.72 s** |
| PD 传输 | prefill 侧 `Found 8 HCAs` / `Using RDMA transport (RoCE/iWARP)`；decode 侧 `#full token: 512` |
| 遥测闸门 | `prefill.nodes=1 valid=1`、`decode.nodes=1 valid=1`，五项门全非空（`max_running=152`） |

`/health` 在两个 worker 上都 200 是**必要但不充分**的：直连 worker 发请求会被拒
（`Invalid request: Disaggregated request received without bootstrap room id`），
room id 由 Router 下发。只有穿过 Router 的请求才真正走 KV 传输，所以验收必须以它为准。

---

## 1. 版本与形态

| 组件 | 版本 |
| --- | --- |
| 控制面 | `tai-talea 0.9.0-cci` (`0b7b5e4952f81063abfc244e0b76dd4ac475d397`, 2026-10-26T08:58:17Z) |
| Router | `sgl-model-gateway 0.3.2` |
| sglang | **0.5.22**（不是镜像自带的 0.5.3rc0，见下） |
| torch | 2.14.1+cu130 |
| profile | `ubuntu-22.04 / cuda 12.9 / python 3.12 / sglang 0.5.22`，两台实测 `compatible=True` |

控制面与 Router 跑在 `glm53-pd2`（Router 8000、控制面 8787），两台容器各跑一个
SGLang worker（服务端口 9002、bootstrap 9001、PD bootstrap 8998）。

## 2. 关键前提：镜像自带的 sglang 加载不了这个模型

镜像 `alayanew-public/sglang:20250930` 的两棵源码树都只有 `glm4*`/`chatglm`，
没有 `glm5_next.py`；而模型目录没有 `auto_map`、没有任何 `*.py`，`--trust-remote-code`
是空操作。所以必须换 venv 里的 sglang 0.5.22。

运行时通过 `cci-unpack-runtime.sh` 从 `/root/gongji/tai-talea-runtime/` 铺装，
**实测 1 分 46 秒**（对比 25 分钟 PyPI 下载）。详见
`alayanew-runtime-and-private-image-2026-10-26.md`。

## 3. 这次踩掉的两个坑

### 3.1 `--model-loader-extra-config` 的引号被 shlex 吃掉

配置写 `--model-loader-extra-config={"concurrency":16}`，bootstrap 的
`validate_extra_args` 做 `shlex.split` 后双引号丢失，sglang 拿到 `{concurrency:16}`，
`orjson` 在启动 20 秒后报
`unexpected character, expected a string key: line 1 column 2 (char 1)`。

一次尝试就这么没了（`start_max_attempts` 那时是 3）。正确写法：

```yaml
- "--model-loader-extra-config='{\"concurrency\":16}'"
```

**验证方法**：不要靠读配置猜。容器上有 `check-args.py`，直接调用 bootstrap 自己的
`validate_extra_args` + `build_sglang_command` 打印真实 argv，能看到 `<- valid JSON`。

### 3.2 控制面在启动途中重启 → 孤儿进程与假 FAILED

重启控制面后 `STARTING` 的所有权（在内存里）丢失，实例被记成
`start operation was interrupted: no owner for STARTING state`，**但 bootstrap 那边的
sglang 还在正常加载**。于是出现「控制面说 FAILED、进程在跑」的两份真相。

本次就是这个形态：pd3 被判 FAILED，实际 19:52 正常加载完并 `SERVING`。
判据只能看容器里的 `ps` 与 `sglang-*.log`，不能只看控制面状态。

另外 `pkill` 后立刻起新进程会让 SQLite 短暂锁住，启动恢复报
`list instances: context deadline exceeded`，随后自愈（查询 <1 ms），不需要干预。

## 4. 加载耗时（两台并发）

| 阶段 | 时间 |
| --- | --- |
| 派发 → `Load weight begin` | ~1 min |
| 权重加载 76108 张量 / 305.8 GiB | ~19 min（两台并发，各约 73 张量/s，合计 ~146/s） |
| 图捕获 | prefill 42 shape ~43 s；decode 20 batch ~70 s |
| **合计** | **约 20 分钟**（19:34:41 派发 → 19:54:09 就绪） |

早期采样曾误判为「两台并发要 40 分钟以上」：前几分钟在扫元数据，张量速率只有
10/s 上下，进入流式阶段后稳定在 73/s。CephFS HDD 池在 32 条并发流下没有成为瓶颈。

## 5. token 预算

内存池：prefill `max_total_num_tokens=1346560`，decode `max_total_num_tokens=2351104`；
`--max-running-requests 128`（仅 decode）、`--cuda-graph-max-bs-decode 128`。

## 6. 复现步骤

```bash
# 1. 新容器铺运行时（2 分钟）
bash /root/cci-unpack-runtime.sh

# 2. 重启 bootstrap，让它重新探测环境（旧的会缓存 0.5.3rc0）
bash /root/cci-bs-restart.sh

# 3. 配置预检 + 启用
/root/tai-talea -check-config -config /etc/tai-talea/tai-talea.next.yaml
python3 /root/check-args.py /etc/tai-talea/tai-talea.next.yaml   # 必须先过这步
cp /etc/tai-talea/tai-talea.next.yaml /etc/tai-talea/tai-talea.yaml

# 4. Router + 控制面
bash /root/cci-start-plane.sh

# 5. 验收（必须穿过 Router）
bash /root/cci-chat-test.sh
```

## 7. 尚未做的

- 镜像化：`imageType: private` 的代码已就绪，等平台开通「混闪-镜像」并给出租户项目名
- 弹性：`planner.mode: fixed`、`capacity_actions: false`，目前不做按需扩缩
- Router 重启后 worker id 全换的问题（`url_to_id` 是进程内 DashMap）未在本平台验证

## 8. 上线后接公网时修掉的 503

把 Router 的 8000 端口开到公网后，第一次真实客户端调用返回：

```
{"error":{"type":"Service Unavailable","code":"server_selection_failed",
 "message":"No available servers: No available decode workers (all circuits open or unhealthy)"}}
```

### 8.1 根因：sglang 的 `/health` 在 PD 模式下会自己骗自己

`srt/entrypoints/http_server.py` 里 `/health` 不是「进程是否活着」，它**发一个真实的
1-token 生成请求**，然后等 20 秒看有没有任何响应：

```python
gri = GenerateReqInput(rid=rid, input_ids=[0], sampling_params={"max_new_tokens": 1, ...})
if get_disagg().disaggregation_mode != DisaggregationMode.NULL.value:
    gri.bootstrap_host = FAKE_BOOTSTRAP_HOST
    gri.bootstrap_room = 0
...
while time.time() < tic + HEALTH_CHECK_TIMEOUT:   # 20s
```

也就是在 `--disaggregation-mode=decode` 下，探针请求被塞了一个**假的 prefill 地址和
room 0**，它会真的去等一个永远不会到达的 KV 传输，20 秒后返回 503。

实测代价：

| 端点 | 耗时 | 副作用 |
| --- | --- | --- |
| `/health` | 恒定 **1.0 s**（代码里 `await asyncio.sleep(1)`） | 每次真的占用一次 prefill+decode 调度 |
| `/server_info` | **9 ms** | 只读进程元数据，不碰模型 |

Router 每 5 秒探一次 `/health`，一旦某次探针卡住就级联：2026-10-26 20:44:13 →
20:45:24（**70 秒**）里唯一的 decode worker 被判定不健康，期间所有请求 503。

### 8.2 处置

Router 换探针：

```bash
/root/sgl-model-gateway launch ... \
  --health-check-endpoint /server_info \
  --health-failure-threshold 5 \
  --health-check-timeout-secs 5
```

`/server_info` 只回答「这个进程还能接受工作」，这正是探针能回答的问题；真正卡死的
worker 由 circuit breaker（连续 10 次请求失败）兜住，而不是由一个合成的生成请求决定。

### 8.3 过程中差点被自己的脚本骗过去

第一版重启脚本用 `pkill -x sgl-model-gateway`，**从未生效**：Linux 的 `comm` 只有
15 字符，实际进程名是 `sgl-model-gatew`，精确匹配什么都打不中。后果是一串假成功：

1. 旧 Router（pid 47368）继续占用 8000；
2. 新进程启动即 panic：`failed to install Prometheus metrics exporter:
   FailedToCreateHTTPListener("Address already in use")`；
3. 脚本 `curl :8000/health` 得到 200 —— **那是旧进程在应答**；
4. 于是脚本报告「修复成功」，实际新参数一条都没生效。

判据必须是**监听者 PID 等于刚启动的 PID**，而不是「端口有人应答」：

```bash
OWNER=$(ss -lntp | grep ':8000 ' | grep -o 'pid=[0-9]*' | cut -d= -f2)
[ "$OWNER" = "$NEWPID" ] || echo "new flags did not take effect"
```

另外 `setsid` 会 fork，`$!` 拿到的不是最终 PID，启动后要按 cmdline 重新查。

### 8.4 重启 Router 不会留下空成员表

`RegisterWorker` 只在 start 流程里调用，看起来重启 Router 会变成 0 worker。实际不会：
`reconcile.go` 的 `reconcileServiceMembership` 每轮都会比对 Router 成员，缺失就重新注册，
并记 `service_reregistered` / `service_membership_repaired`。实测重启后 **25 秒内**
两个 worker 自动回位。

### 8.5 另一个不是故障的现象：`content` 为空

本模型是 reasoning 模型，`max_tokens` 给小了会把预算全烧在 `reasoning_content` 上，
`content` 是空串且 `finish_reason=length`。同一条「介绍一下你自己」：

| max_tokens | 结果 |
| --- | --- |
| 512 | `finish=length`，1021 字思考，`content=''` |
| 2048 | `finish=stop`，1023 思考 + 452 字回答，13.6 s |

`chat_template_kwargs: {"reasoning_effort": "low"}` 帮不上忙 —— 模板只接受
`low`/`high`（默认 `max`），而这类开放问题的思考量本来就大。**给客户端的建议是
`max_tokens >= 2048`**，或者直接从 `reasoning_content` 取内容。
