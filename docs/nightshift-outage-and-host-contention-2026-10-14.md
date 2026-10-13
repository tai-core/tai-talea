# nightshift 停摆与宿主机争用（2026-10-14）

## 0. 结论速览

| # | 项 | 结果 |
| --- | --- | --- |
| 1 | 服务状态 | **0P0D 全停**：Router 两个 worker `healthy=0`，`/readiness` 503，经 Router 推理三次全 503 |
| 2 | 控制面/Router | **本身健康**（Pod 24h/22h，0 重启）；停的是它管的两个 worker |
| 3 | prefill（h100-04） | `IDLE/FAILED`，`start_attempts=3` 已烧完 ⇒ **控制面不会自己再试** |
| 4 | decode（h100-01） | `LOST/FAILED`；其 sglang 父进程**还活着但 8 个 scheduler 全是僵尸**、9002 无监听 |
| 5 | **根因** | 两台机器的 GPU 被**宿主上的第三方 docker 容器**占着（h100-04 63GB/卡、h100-01 峰值 57.5GB/卡） |
| 6 | 何时坏的 | 10-13 **09:11**（prefill）/ **13:52**（decode），不是现在才坏 |
| 7 | 第三方活动 | **正在进行中**：10-13 15:56 起陆续起了 4 个 `h100-tune-*` 容器，最近一个 10-14 **00:07** |

---

## 1. 此刻的实况

快照取自 2026-10-14 00:08 CST。

```
=== control plane (10.222.49.107:8787) ===
  talea-cmcc-k8s-1010-172229     IDLE  svc=FAILED  role=prefill a=3 worker=035c6795
  talea-cmcc-k8s-1010-172229-1   LOST  svc=FAILED  role=decode  a=0 worker=ae8f3a63

=== router /workers ===
  prefill_count=1 decode_count=1 regular=0 pending=0
  http://10.214.81.70:9002  type=prefill healthy=False id=035c6795
  http://10.214.81.67:9002  type=decode  healthy=False id=ae8f3a63

=== router /state/workers ===
  http://10.214.81.70:9002  state=unavailable reason=manual gen=22
  http://10.214.81.67:9002  state=unavailable reason=manual gen=25

=== router /readiness ===
  503 {"status":"not ready","reason":"insufficient healthy workers"}

=== inference through the router ===
  503 ×3  server_selection_failed / No available prefill workers (all circuits open or unhealthy)
```

Pod 层面：

| Pod | 节点 | 状态 | 重启 | 起于 |
| --- | --- | --- | --- | --- |
| `tai-talea`（控制面） | h100-04 | Running 1/1 | 0 | 10-12 15:31:54Z |
| `talea-router` | h100-04 | Running 1/1 | 0 | 10-12 17:06:07Z |
| `talea-cmcc-k8s-1010-172229`（worker） | h100-04 | **Running 0/1** | 0 | 10-10 17:22:29Z |
| `talea-cmcc-k8s-1010-172229-1`（worker） | h100-01 | **Running 0/1** | 0 | 10-10 17:22:32Z |
| `watch`（我们的旧观察者） | h100-04 | Running 1/1 | 0 | 10-12 15:35:44Z |

注意 `reason=manual`：这是**控制面自己**通过 `PUT /state/workers/{id}` 摘的流（实例 FAILED/LOST 时它本来就应该这么做），**不是**别人在摘。Router 侧是结果，不是原因。

控制面每 5 分钟在报 `instance_lost_rate_high`（critical，`active=2 lost=1 rate=0.500 threshold=0.200`）。

---

## 2. 时间线（CST，从 `watch` 观察者的 24h 日志重建）

观察者 Pod `watch` 起于 10-12 15:35:44Z，正好把整个过程录下来了。**它的时间戳是 UTC**，下表已换算。

| 时刻（CST） | 事件 |
| --- | --- |
| 10-12 23:35:45 | 系统**健康**：`172229:prefill/0f953c25` `172229-1:decode/038e3175`，双 IDLE/SERVING，`healthy=2`，readiness **ready**，**CHAT=200** |
| 10-13 00:13:47 | **D2 摘流测试**：decode → `DRAINING` |
| 10-13 00:17:17 | decode 回 `SERVING` ⇒ **摘流/恢复这轮是通过的** |
| 10-13 01:06:17 | **Router 重启**（`/workers` connection refused）⇒ worker id 轮换 `038e3175→b32f747f`、`0f953c25→035c6795`，短暂 FAILED 后自愈 |
| 10-13 01:14–01:18 | 角色重排，恢复 `healthy=2 / ready` |
| 10-13 08:45 | prefill 报 `running model is not healthy: status=degraded`，1 分钟后自愈 |
| 10-13 **09:11:23** | prefill 崩：`sglang process exited: sglang exited unexpectedly with code -9` |
| 10-13 09:11–09:30 | 反复重启 4 次，全部 `-9`，`start_attempts` 烧到 **3** |
| 10-13 **09:30:53** | prefill 进入 `FAILED(a=3)` —— **从此永久停摆** |
| 10-13 13:48:44 | decode 最后一次拉起 sglang（父进程 248772） |
| 10-13 13:52:26 | decode `sglang process is not running: bootstrap phase STOPPING` |
| 10-13 **13:52:56** | decode → **LOST**（`container endpoint is unreachable`） |
| 10-13 15:56 | 第三方 `/h100-tune-bench` 在 h100-04 启动 |
| 10-13 23:15 | 第三方 `/h100-tune-epd-decode` 在 h100-04 启动 → **占满 8 卡 63GB** |
| 10-14 00:00 / 00:03 / 00:07 | 第三方 `/h100-tune-epd-prefill`（h100-01）、`/h100-tune-epd-router`（h100-04）陆续启动；prefill 还重建过一次 |

⇒ **从 10-13 09:30 起，prefill 就没有再起来过；13:52 起 decode 也没了。已经停了约 10.5 小时。**

---

## 3. 两个 worker 的死法不一样

**prefill（h100-04）**：`start_attempts` 用完了。

```
last_error = sglang health check failed: sglang process failed during startup:
             sglang exited unexpectedly with code -9
```

`-9` 是 SIGKILL。查 `dmesg` **没有**近期的 OOM kill 记录（最后一次是 10-09 18:54，被杀的是别人的 `VLLM::Worker_TP`）⇒ 不是内核 OOM killer 干的。更像是 sglang 自身的 watchdog 在 GPU 被抢占/调度停顿时把它杀了，而这个平台上 `--watchdog-timeout` **不在 bootstrap 白名单里**，我们没法调。

**decode（h100-01）**：父进程活着、子进程全死。

```
PID    PPID   STARTED              ELAPSED   STAT  RSS
248772 597384 Tue Oct 13 13:48:44  10:19:51  Ssl   1624880 kB  (-m sglang.launch_server --port 9002 --disaggregation-mode decode)

其子进程：
  249360 S   resource_tracker
  249361 Z   [sglang::schedul] <defunct>
  249609 Z   [sglang::schedul] <defunct>
  ...（8 个 scheduler 全部 <defunct>）

$ ss -lntp | grep :9002   →   （空）
```

即：**父进程是"僵尸父母"** —— 8 个 scheduler 早已退出且没被回收，9002 没有任何监听。控制面判它 LOST 是**正确**的。

---

## 4. 机器现在归谁：宿主上的第三方 docker 容器

关键判据是 **cgroup 路径**：K8s Pod 的进程在 `/kubepods/...` 下，而这批在 `/system.slice/docker-<id>.scope` 下 —— 也就是**在宿主上直接 `docker run` 起来的，不在 K8s 里**。

h100-04（我们的 prefill 节点）：

| 容器 | 镜像 | 启动（UTC） |
| --- | --- | --- |
| `/h100-tune-epd-router` | `sglang:0.5.21-glm53-h100-final-20261014` | 2026-10-13T16:03:04Z |
| **`/h100-tune-epd-decode`** | `sglang:0.5.21-glm53-full600-epd-h100-20261013` | 2026-10-13T15:15:26Z |
| `/h100-tune-bench` | `sglang:0.5.21-glm53-h100-bench-evalscope112-20261013` | 2026-10-13T07:56:09Z |
| `/glm-chy` | `vllm-glm53-fp8:stable-2bc14a8-sm90` | 2026-09-24T08:19:59Z |
| `/eval-chy` | `.../aiops-dev/vllm:lightly-0921` | 2026-09-24T09:03:12Z |

h100-01（我们的 decode 节点）：

| 容器 | 镜像 | 启动（UTC） |
| --- | --- | --- |
| `/h100-tune-epd-prefill` | `sglang:0.5.21-glm53-h100-final-20261014` | 16:00:07Z → **重建于 16:07:19Z** |

**显存归属（两次采样，间隔 90 秒）**：

| 节点 | 16:06:27 UTC | 16:07:57 UTC |
| --- | --- | --- |
| h100-04 | 63.2 GB / 81.5 GB × 8 卡 | 63.2 GB / 81.5 GB × 8 卡（**没变**） |
| h100-01 | 57.5 GB / 81.5 GB × 8 卡 | **~0.5 GB**（4 MiB on 4 卡） |

h100-04 上唯一的 sglang 进程树（`164123` → `164255` → 8 个 `scheduler_DP*_TP*_EP*`）**全部在 `docker-714b0b52…scope` 里**，也就是 `/h100-tune-epd-decode`；**没有任何 sglang 在 `/kubepods/` 下**。它的参数也完全不是我们的配置：

```
--served-model-name glm-5.3-flash --dp-size 4 --port 32410
--disaggregation-mode decode --disaggregation-bootstrap-port 32405
--disaggregation-transfer-backend mooncake
--speculative-algorithm EAGLE --mem-fraction-static 0.70
```

（我们的配置是 `--port 9002`、无 EAGLE—— 两者互斥。）

h100-01 那 57.5 GB **不是我们的**：我们的父进程 248772 在两次采样之间一直活着、scheduler 全是僵尸、`VmRSS` 只有 1.6 GB，而显存是在**外来容器重建的那一分钟**掉下去的。

---

## 5. 为什么控制面不会自己恢复

两条独立的刹车，缺一不可：

1. **prefill 的 `start_attempts` 已经烧到 3。** `reconcile` 对烧完的实例直接放弃，`rebalance` 又**看不见 FAILED 实例** ⇒ 不重置尝试次数就不会再拉起。
2. **decode 是 LOST。** `finishDrain` 对 LOST 会直接记 stopped（不去调 `bootstrap/stop`，因为那个端点在自己网络里不答），而它那个僵尸父进程如果不杀掉，下次启动会撞上同一个 9002 端口/同一批已死的 scheduler。

再加上：**只要机器上的外来容器占着显存，拉起也是白拉**（prefill 的 `--mem-fraction-static 0.85` 要 ~63GB/卡）。

---

## 6. 恢复需要做什么（待决定）

按依赖顺序：

1. **处理外来容器**（h100-04 上 5 个、h100-01 上 1 个）。至少 `h100-tune-epd-decode`/`-epd-router`/`-epd-prefill` 是**今天才起、正在抢占**的。用户此前有过授权（"现在机器归我们使用，其他人的进程你可以直接杀了"），但这批是**新的**、而且包含 09-24 起就常驻的 `glm-chy`/`eval-chy`，动手前需要再确认范围。
2. **清掉我们自己的僵尸父进程**（h100-01 的 248772 及其 8 个 defunct scheduler），否则 decode 起不来。
3. **重置 prefill 的 `start_attempts`**（或在注册表里删行重加），让控制面愿意再试一次。
4. 然后等控制面把两个角色重新拉起（NAS 模型加载约 15–20 分钟/台），再验 `healthy=2 / readiness=200 / CHAT=200`。
5. 顺手：`watch` 观察者已连续跑 24 小时，恢复期间可以留着当时间线，验完清理。

---

## 7. 本次新增的观测工具

都在 `.workbuddy/tmp/alayanew/`：

| 工具 | 用途 |
| --- | --- |
| `nrsh.py <script.sh>` | 把**本地脚本文件**送到 n1 执行。`nsk.py` 会把 argv 用空格拼起来，命令里的引号会被吃掉（含空格的 grep 模式会被拆成两个参数）；走文件就没这个问题 |
| `ns-gpu-only.py` | 只看外来容器 + 显存占用（**关键段打印在最后**，不会被 `tail` 截掉——`ns-host-truth.py` 就吃过这个亏，dmesg 太吵） |
| `ns-host-truth.py` | 外来容器 + 显存 + `dmesg` OOM/Xid |
| `ns-who-owns.py` | 用 **cgroup 路径**判定 sglang 属于哪个 Pod（hostNetwork 下看 IP 和命令行都分不出敌我） |
| `ns-decode-state.py` | 判定某个 sglang 是"活着在服务"还是"僵尸父母"（看 scheduler 的 `STAT` 和 9002 有没有监听） |
| `ns-changes.sh` | 把 `watch` 日志压成**状态变化点**（按摘要行去重） |
| `ns-name-pods.sh` | cgroup UID → Pod 名字，并列出两个 GPU 节点上的全部 Pod |

**判据**：`/kubepods/...` = K8s Pod；`/system.slice/docker-*.scope` = 宿主机 `docker run`。
