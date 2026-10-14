# nightshift 控制面 v0.7 上线（2026-10-15）

> 承接 `self-healing-liveness-and-start-budget-2026-10-15.md`（B + A 的代码交付，其中 §8 列出四项
> 「生效前置」）。本篇只做**其中控制面那一笔**：把 A2/A3 随镜像推上线，并打开 A1 的两个开关。
> **B 未动**（探针需要重建 worker 镜像，见 §8）。
> 时间一律 **UTC**（CST = UTC+8）。

---

## 0. 结论速览

| # | 项 | 结果 |
| --- | --- | --- |
| 1 | 滚动 | v0.6 → **v0.7**，`imageID` 与推送的 manifest digest **逐字一致** |
| 2 | 镜像自证 | 镜像内 `--version` 输出 `tai-talea v0.7 (be3162b, 2026-10-14T18:22:18Z, go1.27.1)`，即**已提交的那个 revision** |
| 3 | 预检 | 新镜像对**候选配置**跑 `--check-config` → exit 0（`1 partners, router http://10.214.81.70:8000`） |
| 4 | 迁移 | schema v3 → v4 已在**活的库**上完成；接口开始返回 `start_rearms` / `start_parked_at` |
| 5 | 接管 | `control plane recovery finished checked=10 lost=0 recovered=0 reregistered=0 unhealthy=0` |
| 6 | 服务 | Router `ready`、`p=1 d=1 healthy=2`；真实推理 **3/3** 通过（0.48s / 0.97s / 1.88s） |
| 7 | 窗口内错误 | 滚动后 `WARN`/`ERROR` **零条** |
| 8 | A1 | 已打开（`start_retry_cooldown: 30m` + `start_retry_max_rearms: 1`），但**尚未被真实触发** —— 见 §6 |
| 9 | 未做 | **B**（`liveness_probe`）保持 `false`：worker 镜像仍是 `talea-worker:v0.2`，没有 `/bootstrap/liveness` |

---

## 1. 这一版带了什么

代码在 `238180f`（`controller: recover a silent worker, and a start budget that ran out`），
本次只是把它送上线。

| 能力 | 生效方式 | 本次是否生效 |
| --- | --- | --- |
| **A2** 停摆实例对 planner 可见（`Recovering` / `Stranded` 两个只读桶 + `PROVISION` 护栏） | 随镜像 | ✅ 已生效 |
| **A3** LOST 行按**准备预算**判据（不再误报为「正在恢复」） | 随镜像 | ✅ 已生效 |
| **A1** 烧完的启动预算冷却后重臂 | 需配置两个开关 | ✅ 本次打开 |
| **B** worker 的 `livenessProbe` | 需重建 worker 镜像 + 换 tag + 重建 Pod | ❌ 未做 |

A2/A3 随镜像即生效、无需配置，因为它们**只改判据与可见性**，不新增任何破坏性动作 —— 这一点是
上一轮设计时定的，也是决定「先做控制面」的依据：这一笔的爆炸半径接近零。

---

## 2. 镜像

```
tag             shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn/library/tai-talea:v0.7
manifest digest sha256:cd60180c3711c87da3b7c662208a9d9259730c60b0acf73b4b13d17593d606d9
layer           sha256:1ef7257d98dddb344bc9dee3b66f06ed6d10735c190e26f6707c638652adc20d  (5 592 772 B)
diff_id         sha256:fec54f5f53c7267d29275513ac4bbe63101016991d56781ac6cbeea482dea15a
binary          13 267 104 B, CGO_ENABLED=0 GOOS=linux GOARCH=amd64, -trimpath, -s -w
```

镜像用 `talea-control-image.py`（纯 stdlib 的 OCI 构建，无 docker）在**本机**构建与推送；
烤进去的是仓库里的**模板** `deploy/kubernetes/tai-talea.yaml`，不是线上那份 —— 线上那份带真实
token，而这个 registry 允许匿名拉取。已核对：模板里只有 `change-me-*` 与 `cmcc-k8s-push-*` 占位符。

**一个值得记下来的差异**：v0.6 的二进制造出来时 `vcs.modified=true` —— 它是从 `a5148ec`
**脏工作树**编的，所以镜像里跑的代码与仓库当时任何 commit 都不对应。v0.7 是 `vcs.modified=false`，
`vcs.revision=be3162bb57ec…` 即 HEAD。本次还加了 `-trimpath`（去掉二进制里的本机路径）。
`go version -m` 在 `-trimpath` 下**不打印 `-ldflags` 行**，别据此以为版本没注进去 ——
判据是二进制里真的有 `v0.7` / `be3162b`，以及 §5 的 `--version`。

---

## 3. 配置改动

改的是 **h100-04 上的线上那份** `/data/tai-talea/config/tai-talea.yaml`，不是仓库模板。

| | sha256 | 字节 |
| --- | --- | --- |
| 改前 | `c534a3ec42cd922c99322d596d5cad7212ae1503f8e7b80ba32a893f4ed33562` | 14 106 |
| 改后 | `a84de79fc9c9a478db30500743c629ccf6a2cd0c7be277fda448318b434a64f4` | 16 483 |
| 备份 | `tai-talea.yaml.bak-before-v07`（内容 = 改前那份） | |

新增四项 —— 前三项是**把已经在用的默认值写出来**（此前靠 `Default()` 兜底，配置文件里看不见），
第四项才是真的行为改变：

```yaml
controller:
  start_retry_cooldown: 30m      # ← 行为改变
  start_retry_max_rearms: 1      # ← 行为改变
  health_failure_threshold: 3    # 写出来，值不变（v0.6 起就靠默认值在生效）
router:
  removal_timeout: 8m            # 写出来，值不变
partners[].kubernetes:
  liveness_probe: false          # 写出来，开关位置可见（B 的入口）
```

### 3.1 为什么是 30m / **1**，而不是设计文档里建议的 3

设计文档 §8 写的是 `start_retry_max_rearms: 3`。这里用了 **1**，理由：

- 这是重臂机制**第一次**在真实生产上跑。它的存在意义是消除「预算烧完 = 永久停摆」这个故障形态，
  而**一次重臂就已经消除了它**。
- 上限直接决定最坏代价：每次尝试都可能是一次 8 卡全量权重加载。
  `max_rearms=1` ⇒ 最坏 6 次尝试；`max_rearms=3` ⇒ 最坏 12 次。
- 等**观察过一次重臂真的成功**之后再把上限抬上去，那时这个数字是有依据的，而不是一个猜测。

改一行即可调整。**这是一处我替你做的判断，不是设计文档里的原值。**

### 3.2 A1 对已有行完全无副作用

`start_parked_at` 是**新列**，所有历史行都是零值；而 `startRearmDue` 明确要求它非零。
⇒ 只有**升级之后**才烧完预算、并且由 `failStart` 打上停摆时间戳的行才会被重臂。
现网那 10 行（含 2 个在服务的）不会有任何行为变化 —— 这一点在滚动后由接口数据直接证实：
所有行的 `rearm=0`、`parked=0001-01-01T00:00:00Z`。

### 3.3 写入顺序：先写侧路径、再提升

旧二进制配置解码是 `KnownFields(true)`，**不认识** `start_retry_*`。所以如果先把新配置覆盖到
live 路径，就会出现一个窗口：旧 Pod 万一在这段时间里重启（`restartPolicy=Always`），它会因为
无法解析配置而**启动失败**。

因此流程是：

1. 新配置写到 `/data/tai-talea/config/tai-talea.next.yaml`（**live 文件不动**）；
2. 用**新镜像**对 `next.yaml` 跑 `--check-config`；
3. exit 0 之后才 `cp` 覆盖 live（同时留 `.bak-before-v07`），并删掉 next 文件；
4. 立刻滚动。

这样 live 配置**从未**处于「旧二进制读不了」的状态。

---

## 4. 滚动与证据

`ns-deploy-cp.py`（Pod，不是 Deployment，没有 rollout 机制）：删旧 Pod → 轮询等它**真的消失**
（`--wait=false` 立刻返回，而对着 Terminating 对象 apply 可能被拒）→ apply → 等 Running 且 Ready
→ **逐字比对 `imageID`**（tag 不是证据，`imagePullPolicy: Always` 只保证 tag 背后是最新 digest）。

```
### before      .../tai-talea:v0.6   phase=Running restarts=0
### deleting tai-talea
   vanished: True
### applying
pod/tai-talea created
### after: phase=Running ready=true
### imageID  .../tai-talea@sha256:cd60180c3711c87da3b7c662208a9d9259730c60b0acf73b4b13d17593d606d9
### wanted   sha256:cd60180c3711c87da3b7c662208a9d9259730c60b0acf73b4b13d17593d606d9
### MATCH: the running container is the build that was pushed
```

预检**第一次是失败的**，而且失败得很有价值：MSYS 把 `--config /etc/tai-talea/…` 改写成了 Windows
路径，Pod 报 `read config D:/wb/…: no such file or directory`、exit 1。也就是说 ——
**一个坏配置确实会让预检拒绝滚动**，这正是它存在的意义。加 `MSYS_NO_PATHCONV=1` 后：

```
### phase=Succeeded exit_code=0
### logs
configuration /etc/tai-talea/tai-talea.next.yaml is valid (1 partners, router http://10.214.81.70:8000)
### PREFLIGHT OK
```

---

## 5. 验收

**启动日志**（新 Pod `10.222.49.127`，`startTime 2026-10-14T18:25:56Z`，restarts 0）：

```
planner configuration  mode=fixed gear=1:1 capacity_actions=false provision_timeout=45m0s
control plane recovery finished  checked=10 lost=0 recovered=0 reregistered=0 unhealthy=0 resumed_operations=0 resumed_events=0
partner pull enabled  partner_id=cmcc-k8s interval=30s
capacity control plane listening  address=0.0.0.0:8787 router=http://10.214.81.70:8000
```

`lost=0` 是关键：接管没有把任何实例判死。滚动后 `WARN`/`ERROR` **零条**。

**镜像自证**（用该镜像跑 `--version`）：

```
tai-talea v0.7 (be3162b, 2026-10-14T18:22:18Z, go1.27.1)
```

**控制面视角** —— 新字段出现即迁移落地，且历史行未被扰动：

```
talea-cmcc-k8s-1010-172229     IDLE  svc=SERVING  role=prefill  attempts=0 rearm=0 parked=0001-01-01T00:00:00Z worker=d4045cba
talea-cmcc-k8s-1010-172229-1   IDLE  svc=SERVING  role=decode   attempts=0 rearm=0 parked=0001-01-01T00:00:00Z worker=6706184e
（另有 8 行 RELEASED，同样 rearm=0 / parked 为空）
```

**Router 与推理**（滚动前后一致）：

```
/readiness -> {"status":"ready","healthy_workers":2,"available_workers":2,"total_workers":2}
stats={'prefill_count': 1, 'decode_count': 1}
   http://10.214.81.67:9002  type=decode   healthy=True  load=0
   http://10.214.81.70:9002  type=prefill  healthy=True  load=0

--- arithmetic -> 200 in 0.48s   completion_tokens=16
--- chinese    -> 200 in 0.97s   completion_tokens=64
--- longform   -> 200 in 1.88s   completion_tokens=160
### verdict: 3/3 completions returned real, metered output
```

**观察者**：`watch` Pod 的 `CTRL` 是**启动时解析一次**的（无 hostNetwork ⇒ 每次滚动换集群 IP），
所以它跨不过滚动 —— 已删除并按原参数重启，现指向 `http://10.222.49.127:8787`，首行即为：

```
t+0s | 172229=IDLE/SERVING/prefill(a=0) 172229-1=IDLE/SERVING/decode(a=0) | router p=1 d=1 healthy=2 | readiness=ready | CHAT=200 …
```

---

## 6. A1 已打开，但**尚未被真实触发**

必须说清楚：本次**没有**制造一次预算耗尽的现场（那要牺牲 8 卡上的一个 worker）。所以
「重臂在真实集群上确实会按 30m 冷却重新尝试」这条结论，目前只有**单元级**证据
（`internal/controller/rearm_test.go` 6 条，覆盖：默认不重臂 / 冷却内不重臂且时间戳不动 /
冷却后 `attempts=0 rearms=1 park 清空` 并写审计 / 不推进时钟无法二次重臂 / 重臂后能恢复 SERVING
并清债 / 上限用满后永不再重臂），**没有生产证据**。

而且它的触发条件很窄：需要某个实例在**升级之后**连续失败 `start_max_attempts=3` 次。
现网 2 个 worker 都在 `SERVING`、`attempts=0`，所以短期内大概率碰不到。
**第一次真实重臂发生时，那就是它的第一份生产证据** —— 审计事件是 `service_start_rearmed`（带 `parked_for`）。

---

## 7. Rollback

| 想回退什么 | 怎么做 | 注意 |
| --- | --- | --- |
| 只回退 A1（关掉自动重臂） | 把 `start_retry_cooldown` 改回 `0s`，重启 Pod | 安全；旧行本来就没有 park 时间戳 |
| 回退整个配置 | `cp /data/tai-talea/config/tai-talea.yaml.bak-before-v07 …/tai-talea.yaml` 后重启 | 备份 = `c534a3ec…` |
| 回退镜像 | manifest tag 改回 `v0.6` 再 `ns-deploy-cp.py` | **v0.6 能读这份新配置吗？不能** —— 它不认识 `start_retry_*` 会启动失败，必须**同时**回退配置到 §3 的备份 |
| 迁移 | **不用回退**。v3→v4 只**加列**，旧二进制不读这两列，忽略即可 | 数据不丢 |

⚠️ 上面第 3 条是最容易做错的一步：**镜像与配置必须一起回退**，因为配置解码是严格的。

---

## 8. 仍然没做

**B（`livenessProbe`）**—— 这是自愈的另一半，也是「冻住的容器自己回来」那一条。

- 线上 worker 镜像仍是 `talea-worker:v0.2`，**没有** `/bootstrap/liveness`（该端点随本仓
  `bootstrap/` 进镜像，v0.2 建于它存在之前）。
- 配置里 `liveness_probe: false`。**不能只改这一行** —— 旧镜像回 404，kubelet 视为失败，
  容器会永远重启且永不服务。
- 要上 B 需要：① 用本仓 `bootstrap/` 重建 worker 镜像推 ecis；② 把
  `partners[].kubernetes.image` 换到新 tag；③ `liveness_probe: true`；④ 重建 worker Pod
  （一次 drain + 约 7 分钟权重重载）。①②③ 必须在**同一个改动**里。
- 顺带欠债：`bootstrap_version` 仍是 `tai-talea-bootstrap/1`。加端点其实是契约变化，应升 `/2`，
  否则「旧镜像 + 新探针」是**无声的 CrashLoop** 而不是显式错误。正确顺序是**随镜像重建一起升**。

> **更新（2026-10-15，同日晚）**：这一节的欠债已经还掉了一半，记录见
> [`talea-worker-v03-and-the-liveness-contract-2026-10-15.md`](talea-worker-v03-and-the-liveness-contract-2026-10-15.md)。
> worker 镜像 `v0.3`（增量层，19 KB）已推送并在真机上验透，`bootstrap_version` 已升 `/2`，
> 控制面 `v0.8` 已预检通过。但**控制面没滚、worker Pod 没重建**——因为滚控制面单独的收益是零
> （探针写在 Pod spec 里，不重建 Pod 就不生效），代价却是在线 worker 会在一次探针抖动里被判死
> 且自愈不了。两件事必须合并进同一个窗口，见该文档 §5 与 §7。

**另一个遗留**：`start_parked_at` 的零值在接口里被序列化成 `"0001-01-01T00:00:00Z"` 而不是省略 ——
Go 的 `omitempty` 对 `time.Time` 这种结构体不生效。语义无歧义，但读起来难看；要修就把它换成
`*time.Time` 或在 DTO 层格式化。纯外观问题。
