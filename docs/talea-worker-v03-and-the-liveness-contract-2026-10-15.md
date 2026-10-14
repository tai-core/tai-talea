# worker v0.3 与 liveness 契约：B 的镜像面就绪，上线等窗口（2026-10-15）

> **状态（已更新）**：§7 的四步窗口**已于 2026-10-15 执行完毕**——控制面滚到 v0.8、线上配置提升为
> `v0.3 + liveness_probe: true`、两台 worker Pod 用**新名字**重建。B 已在线上生效：两台 Pod 的 spec 里
> 都有指向 `/bootstrap/liveness` 的 `livenessProbe`，且 `restartCount` 不增长。
> 本文 §1–§6 保留窗口**之前**的判断（含「为什么必须等窗口」那段推理），§7 改为执行记录，
> §10 是 B 的核心主张在真机上的独立证明，§11 是尚未闭环的部分。
>
> **仍未闭环**：1P1D 端到端推理 200 —— 被第三方占用 h100-04 全部 8 张卡阻塞（细节见 §11）。
> 两块 worker 镜像/控制面镜像**都不需要回滚**：回滚点都还在（见 §8）。

---

## 0. 结论速览

| 项 | 状态 | 证据 |
| --- | --- | --- |
| worker 镜像 `talea-worker:v0.3` | ✅ 已推送 | manifest `sha256:e8eba9ed…5afe9e1d` |
| `v0.3` 携带 `/bootstrap/liveness` | ✅ 真机验透 | §3：200 / 401 / 404 三态 |
| 控制面二进制常量 → `/2` | ✅ 已改已提交 | `93063d8`，`cmd/tai-talea/main.go:267` |
| 控制面镜像 `tai-talea:v0.8` | ✅ 已推送、**已上线** | manifest `sha256:e00530bd…8c69195e`；§7.2 |
| 候选配置（v0.3 + 探针开） | ✅ **已提升为 live** | `e7b0e974…`（备份 `tai-talea.yaml.bak-before-v08` = `a84de79f…`） |
| 两台 worker Pod 重建 | ✅ 已重建（**新名字**） | `talea-cmcc-k8s-1014-193606` / `-1`；§7.3 |
| Pod spec 里有 `livenessProbe` | ✅ 两台都有 | `/bootstrap/liveness`，60/10/5/6，带 `X-Bootstrap-Token` |
| 探针不误杀 | ✅ `restartCount` 不增长 | 两台 `phase=Running restarts=0`；§7.4 |
| **B 的核心主张：冻住的树被 kubelet 重启** | ✅ 真机独立证明 | §10：`SIGSTOP` ⇒ `restartCount 0→1→2`、`exitCode 137` |
| decode worker 回到 SERVING | ✅ | `svc=SERVING role=decode worker=6706184e…` |
| prefill worker 回到 SERVING | ❌ **未成（外部原因）** | h100-04 8 卡被第三方容器占死；§11 |
| 1P1D 真实推理 200 | ❌ 未做（同上） | §11 |
| 现网其余部分是否受影响 | ✅ 未受影响 | §6 + §7.4 |

---

## 1. B 必须同时改两处，否则它只是一个没被声明的端点

`/bootstrap/liveness` 加进 bootstrap（`server.py`，`238180f`）本身是微小的：不持锁、不碰模型、不碰磁盘，
所以它在环境准备和权重加载这些合法慢阶段里始终是 200，只在这进程停止服务时掉下去。这正是
`/bootstrap/health` 做不到的——后者要拿 `SGLangService._lock` 再花 2s 问模型，于是「锁被占」「模型 FAILED」
「进程冻住」三种状态长得一模一样（`server.py` 模块注释里写了这段推理）。

但一个没人声明存在的端点等于不存在。**worker 镜像早于它 → kubelet 打过去 404 → kubelet 判定容器已死 →
Pod 永远重启且永不服务**。这个失败方向比它要治的病更糟：一个冻住的 worker 占着 8 张卡，
一个起不来的 worker 什么都不干。

所以契约要能被控制面查到。而控制面唯一能看到的，是容器自己声称了什么：

- **容器侧**：`BOOTSTRAP_VERSION`（`bootstrap/tai_talea_bootstrap/__init__.py`），随 `/bootstrap/health` 的
  `version` 字段报出去（`server.py:179`）。
- **控制面侧**：`const bootstrapVersion`（`cmd/tai-talea/main.go:267`），**编译期常量，不在配置文件里**，
  经 `cmd/tai-talea/main.go:113` 进入 `instancemanager.Profile.BootstrapVersion`。
- **校验点只有一处**：`internal/instancemanager/manager.go:130`，在 `Prepare()` 里。
  不等 ⇒ `ErrBootstrapVersion` ⇒ `DescribeFailure` 映射成 `reason="bootstrap_version_mismatch"`
  + `obs.AlertBootstrapFailure`。

于是 `/2` 的含义不是「版本号涨了」，而是：**v0.8 控制面拒绝任何答 `/1` 的容器**。
这使「探针开着 + 镜像没有端点」这组坏配对在**准备期**就大声失败，而不是在**探针期**静默 CrashLoop。
改名是目的，不是副作用。

`deploy/container/profile.json` 与 helm ConfigMap 里的 `bootstrap_version` 一并改成 `/2`，但那两处
**只是被携带、从不被比较**：`detect_environment`（`profile.py:206-234`）只比
os/cuda/python/sglang/wheelhouse/virtualenv。改它们是为了自洽，不是为了行为。

---

## 2. worker 镜像 v0.3：一个 19 KB 的增量层

### 2.1 为什么不重打整盘

`talea-worker:v0.2` 是一个 **12.16 GiB 的整机 rootfs 快照**（不是 base + bundle 的合成，见
`docs/talea-worker-selfcontained-image-2026-10-09.md` §2：合成反而更大，因为 OCI 层之间无法去重）。
从 2026-10-10 那次同步到 `93063d8`，`bootstrap/tai_talea_bootstrap/` 里**只有两个文件变了**：

```
$ git diff --stat 7079445 HEAD -- bootstrap/tai_talea_bootstrap/
 bootstrap/tai_talea_bootstrap/__main__.py |  8 +++-
 bootstrap/tai_talea_bootstrap/server.py   | 69 +++++++++++++++++++++------
```

（`7079445` 就是 10-10 那次同步落盘的提交；`roles.py` 在它和 HEAD 之间逐字节相同 —— 用
`git diff --exit-code 7079445 HEAD -- .../roles.py` 验过。）

换句话说：**新镜像与旧镜像的差，就是 B 这一件事**。为 60 KB 的 Python 重推 12 GiB 是浪费，
而且节点上那 12 GiB 早就在 content store 里了。

### 2.2 增量层为什么成立

registry blob 按内容寻址，所以新 manifest 可以**原样引用**旧的 12 GiB layer——同一个 digest，
连同它在 config 里的 `diff_id` 一起。新 config 的 `rootfs.diff_ids` 按**应用顺序**列出两个 diff_id
（旧的在前、新的在后），`layers[]` 同序。节点上 kubelet 发现大层已存在，**只拉新层**。

代价是一个额外的义务，脚本里显式断言：**必须确认那个大 blob 还在 registry 里**
（`HEAD /v2/.../blobs/<digest>` 必须 200）。否则 registry 侧一旦 GC，新 manifest 就指向一个不存在的层，
而这件事要到某台机器重新拉取时才暴露。

### 2.3 容器里的路径由什么决定（这一条差点搞错）

新层必须落在 **`/opt/tai-talea/tai_talea_bootstrap/`**，不是 `/opt/tai-talea/bootstrap/tai_talea_bootstrap/`。
证据不是猜的：

1. v0.2 的 config **`Env` 里没有 `PYTHONPATH`**（见 §2.4 的 Env 列表），而 `Cmd` 是
   `/opt/tai-talea/entrypoint.sh serve`，entrypoint 里跑的是裸 `python -m tai_talea_bootstrap`
   （`deploy/container/entrypoint.sh:145`），`WorkingDir=/opt/tai-talea`。
   → 包是靠 **CWD** 解析的，必须直接位于 ROOT 之下。
2. `docs/worker-host-access-and-allowlist-2026-10-10.md` §4.4 记着当时修 v0.2 的手法是
   「把仓库的 `bootstrap/tai_talea_bootstrap/` 同步进容器的 `/opt/tai-talea/`」。
3. 真机确认（§3 输出）：`module file = /opt/tai-talea/tai_talea_bootstrap/__init__.py`，且
   `ls -d /opt/tai-talea/bootstrap/tai_talea_bootstrap` → **No such file or directory**。

`deploy/container/Dockerfile` 用的是另一套布局（`/opt/tai-talea/bootstrap/` + `ENV PYTHONPATH`）——
那是 docker 路径，与本镜像无关。**照 Dockerfile 的路径打包会得到一个看起来成功、实际完全没生效的层。**

### 2.4 产物与 digest

| 项 | 值 |
| --- | --- |
| tag | `shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn/library/talea-worker:v0.3` |
| manifest | `sha256:e8eba9ed91e2385e65a6f0cad97fe71cecaa19f5e1bf92e1b9bea7735afe9e1d` |
| config blob | `sha256:ca4adaea277f823f459aac816b163e09d1ab478a42f61fd2a704cb8adb589577`（1516 B） |
| layer[0] = **复用** v0.2 的整机层 | `sha256:8fcf6e5442ea4f574d02a96039498692b4bc2fc45c57bd4f036de333e282302d`，13,058,723,088 B |
| &nbsp;&nbsp;其 diff_id | `sha256:32a67d55d021646d8c1d47b333ed895eea5b5f62555e01cb28660361252d9418` |
| layer[1] = **新增** bootstrap 层 | `sha256:832e0939f2443ea031cac45cc43b57d695a53143965a55e6b8fe4354c1e8503e`，19,250 B gz / 81,920 B raw |
| &nbsp;&nbsp;其 diff_id | `sha256:1a5dfc0b7003daf39b310515bf760c7e9c028cb39eee0de0b185bf3acc8c2098` |
| 层内容 | 8 个模块（`__init__.py` 的 `BOOTSTRAP_VERSION` 改写为 `/2`）+ 目录条目，`mode 0644 root:root` |

config 是从 v0.2 的 config **逐字段克隆**后只追加 `diff_id` / `history` / `created` 得到的，
所以 `Env`、`Cmd`、`WorkingDir`、`Labels` 与 v0.2 一字不差——大层的语义没有被顺手改掉。

**可复现**：成员 mtime 由 `SOURCE_DATE_EPOCH` 钉住（本次 = `1792003986` = `93063d8` 的提交时刻）。
早先版本用 `time.time()` 或文件 mtime 当时间戳，两者都会让同一份源码产出不同的层 digest，
从而毁掉「重打一遍对 digest」这个唯一可审计的检查。现在连跑两次 build 得到同一个
`832e0939…`。

---

## 3. 端点验证：在真节点上，用一个一次性 Pod

**为什么不能拿在跑的 worker 验**：把新 bootstrap 装进**在跑**的容器需要 `kubectl exec` 或 `kubectl cp`，
两者都被这个集群的 RBAC 拒绝。所以探针自带一个 Pod，再从 `kubectl logs`（允许）里读结论。

它不可能扰动 1P1D：

- **不申请 GPU 资源**，卡上在跑什么它都能起；
- 跑在**自己的网络命名空间**里，所以它的 `:9001` 不是线上那个 worker 的 `:9001`
  （后者经 `hostNetwork` 挂在宿主 netns 上）；
- **从不加载模型**——起 bootstrap、问三个问题、退出。

`h100-04` 上实测（`ns-worker-probe.py --tag v0.3`）：

```
### imageID .../library/talea-worker@sha256:e8eba9ed91e2385e65a6f0cad97fe71cecaa19f5e1bf92e1b9bea7735afe9e1d
=== 1. import resolution from the image WorkingDir ===
module file      = /opt/tai-talea/tai_talea_bootstrap/__init__.py
BOOTSTRAP_VERSION= tai-talea-bootstrap/2
=== 2. environment self-check (the exit-29 gate) ===
{"compatible": true, "environment": {"os":"ubuntu-24.04","cuda":"13.0","python":"3.12",
 "sglang":"0.5.20","wheelhouse":"/opt/tai-talea/wheelhouse","virtualenv":"/opt/tai-talea/venv"},
 "mismatches": []}
=== 3. start the bootstrap for real (entrypoint.sh serve) ===
{"msg":"serving bootstrap tai-talea-bootstrap/2 on 0.0.0.0:9001 (shared secret required)"}
=== 4. the endpoints, through the shared secret ===
/bootstrap/liveness          token=True  -> HTTP 200  {"status": "alive", "version": "tai-talea-bootstrap/2", "uptime_seconds": 54}
/bootstrap/liveness          token=False -> HTTP 401  {"error": "unauthorized", ...}
/bootstrap/health            token=True  -> HTTP 200  {"status": "ok", "phase": "IDLE", "detail": "bootstrap is up", "version": "tai-talea-bootstrap/2"}
/bootstrap/status            token=True  -> HTTP 200
/bootstrap/does-not-exist    token=True  -> HTTP 404
=== 5. is the process still up after all that? ===
bootstrap ALIVE
```

四件事同时被证：

1. `imageID` 与推送的 manifest digest **逐字一致**（tag 不是证据）；
2. 模块从**正确的路径**被解析到，且报 `/2`；
3. 环境自检 `compatible: true` —— 不会 exit 29；
4. `/bootstrap/liveness` **200**，且**不带 token 时 401**。

第 4 条的后半段是本轮唯一的新增部署约束：`manifest.go:201-213` 给探针配了
`HTTPHeaders: X-Bootstrap-Token`，所以探针能用；但**任何只配 path 不配 header 的探针都会拿到 401**，
而 kubelet 把 401 读作探针失败。这个端点是鉴权的，不是匿名的。

### 3.1 探针的时序是常量，不可配

`internal/partners/kubernetes/manifest.go` 里写死：`InitialDelaySeconds=60`、`PeriodSeconds=10`、
`TimeoutSeconds=5`、`FailureThreshold=6`。即**至少 60s 宽限 + 6 次失败 = 最多 110s** 才判死，
而 306 GB 权重的加载期远长于它——但这无关紧要，因为 liveness 不碰模型：加载期它一直是 200。
宽限是给「容器刚起、HTTP 面还没监听」那个窗口的。

---

## 4. 控制面 v0.8：只改一个常量，但它必须被预检

交叉编译（干净树 `93063d8`）：

```
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w -X …/buildinfo.Version=v0.8 -X …/buildinfo.Commit=93063d8 -X …/buildinfo.BuildTime=…"
```

二进制 13,267,104 B。判据不靠 tag：`grep -a` 确认 `v0.8`、`93063d8`、`tai-talea-bootstrap/2` 各出现，
且 **`tai-talea-bootstrap/1` 出现 0 次**。

| 项 | 值 |
| --- | --- |
| manifest | `sha256:e00530bd84b6c0a25b153ef74d3c8ff8776861805bdd921e93b8c6398c69195e` |
| layer | `sha256:fbf211514b598d9d8c22eaf3123ce0066b8b0911cc7b1b299a5283bac64a694b`，5,592,803 B |
| diff_id | `sha256:768f270409e8fc324760c52cd1a685252569b05b94eb5441b866823a205d4600` |

预检（**候选配置走侧路径**，`tai-talea.next.yaml`，live 从未被覆盖）：

```
configuration /etc/tai-talea/tai-talea.next.yaml is valid (1 partners, router http://10.214.81.70:8000)
### phase=Succeeded exit_code=0
### imageID .../library/tai-talea@sha256:e00530bd84b6c0a25b153ef74d3c8ff8776861805bdd921e93b8c6398c69195e
```

配置改动（`cfg-make-next-v08.py`，三处锚点各断言命中恰好一次）：

| 行 | 从 | 到 |
| --- | --- | --- |
| 207 | `image: …/talea-worker:v0.2` | `…/talea-worker:v0.3` |
| 306-310 | 「Leave this off until…」那段注释 | 改写为「已打开」+ 为什么仍然是一个契约 |
| 318 | `liveness_probe: false` | `liveness_probe: true` |

`a84de79f…`(16483 B) → `e7b0e974…`(16831 B)，节点侧备份 `tai-talea.yaml.bak-before-v08`。

---

## 5. 为什么停在这里：`/2` 和「先不重建 Pod」互相抵消

这是本轮最重要的发现，也是把 `deploy/kubernetes/tai-talea-control-plane.yaml` 的 tag
**留在 v0.7**、把 live 配置**留在 v0.2** 的原因。

**滚控制面单独的收益是零。** 探针写在 Pod spec 里、创建时固定，所以
`liveness_probe: true` 对一个已存在的 Pod 完全没有作用——`manifest.go:190-199` 那段注释本身就是这个意思。
要让 B 生效，那个 Pod 必须被重建。

**但滚控制面单独的代价不是零。** 一旦控制面开始期望 `/2`，而线上两个 worker 还在答 `/1`：

- `reconcile.go:160-190`：**LOST 实例只要重新可达，就被强制送回 `InstancePreparing`，下一轮重跑 `Prepare()`**。
  这不是可选路径——注释写着「Never jump straight to SERVING: re-enter the preparation pipeline」。
- `markLost`（`lifecycle.go:1291-1328`）：**没有连续失败门限**。一次探测不可达就判 LOST。
  `lost_rate_threshold`（`reconcile.go:627-654`）只是喂 gauge 和告警的，不是门闩。

两者相乘 ⇒ 在任何「控制面期望 `/2`、worker 答 `/1`」的区间里，**一次探针抖动**（比如一次
`call_timeout`=10s 到期；`/bootstrap/health` 在负载下走一次 2s 模型探测）就足以：
LOST → 可达 → `Prepare()` → `/1 ≠ /2` → `bootstrap_version_mismatch` → 烧完 3 次 prepare 预算 → 再次 LOST + 告警。
而它**自愈不了**，因为镜像还是 v0.2；唯一的出路是重建那个 Pod——也就是被推迟的那一步。

所以顺序不能是「先滚控制面，以后再重建 Pod」。可选的只有两种：

- **一起做**：滚控制面 + 提升配置 + 立即重建两台 Pod（暴露窗口只有分钟级，且期间不再有任何 worker 处于「旧版本在服务」的稳态）；
- **都不做**：控制面留 v0.7、配置留 v0.2，两边自洽，零风险。

本次选了后者。

> 推论（将来复用时要注意）：**控制面必须先于任何 worker Pod 的创建升到 `/2`。**
> 现在这套 staged 状态是自洽的，因为 live 配置里的 image 还是 v0.2；一旦提升配置而没滚控制面，
> 任何新建的 v0.3 Pod 都会在 `Prepare()` 被判死。

---

## 6. 现网未受影响的证据

本轮全部动作都是只读的，或者局限在一次性 Pod 里（staging Pod、preflight Pod、两个 worker-probe Pod，
都跑完即删）。侦察（`ns-recon-v07.py` + `ns-state-v07.py`，2026-10-14T18:58Z）：

```
### control plane
   image        …/library/tai-talea:v0.7
   imageID      …@sha256:cd60180c3711c87da3b7c662208a9d9259730c60b0acf73b4b13d17593d606d9
   ready        true          restartCount 0
### router /readiness -> 200 {"status":"ready","healthy_workers":2,"available_workers":2,"total_workers":2}
   http://10.214.81.67:9002     type=decode   healthy=True  load=0
   http://10.214.81.70:9002     type=prefill  healthy=True  load=0
### instances
   talea-cmcc-k8s-1010-172229       state=IDLE  svc=SERVING  role=prefill  attempts=0 rearm=0
   talea-cmcc-k8s-1010-172229-1     state=IDLE  svc=SERVING  role=decode   attempts=0 rearm=0
```

`attempts=0 rearm=0 parked=0001-01-01…` 同时说明：A1 自 v0.7 上线以来**仍然从未被真实触发**
（与 `docs/nightshift-control-plane-v07-deploy-2026-10-15.md` §6 一致）。

live 配置 sha256 仍是 `a84de79f…`，与 v0.7 那次滚动留下的字节相同——没有隐性漂移。

---

## 7. 窗口：已执行（2026-10-15）

计划中的四步，**顺序是有约束的**（约束的理由见 §5）。下面是计划与**实际发生的事**。

### 7.0 预备核查（零风险，全绿）

| 对象 | 判据 |
| --- | --- |
| `tai-talea.yaml`（live） | `a84de79f…` = v0.2 + `liveness_probe: false` |
| `tai-talea.next.yaml`（候选） | `e7b0e974…` = v0.3 + `liveness_probe: true` |
| `tai-talea.yaml.bak-before-v07` | `c534a3ec…` |
| `tai-talea.yaml.bak-before-v08` | `a84de79f…` |
| 控制面两个 tag 的 digest | 逐字匹配（`v0.7 → cd60180c3711…`、`v0.8 → e00530bd…`） |
| `talea-worker:v0.3` 的两个 layer blob | `HEAD` 全 200（12.16 GiB `8fcf6e54…` + 19 250 B `832e0939…`） |

最后一条是 §2.2 那个「增量层义务」的兑现检查：如果 registry 侧 GC 掉了复用的 12 GiB blob，
这一步就会失败而不是等到某台机器重新拉取时才炸。

### 7.1 提升候选配置 ✅

`stage-config-v07.py promote`，带 `TALEA_CFG_BACKUP=tai-talea.yaml.bak-before-v08`。
live 变成 `e7b0e974…`、备份保留 `a84de79f…`、候选文件已删除、`exit=0`。

**暴露窗口从这一刻开始**（此刻 v0.8 还没滚，而 live 配置已经指向 v0.3）——所以下一步必须紧接着。

### 7.2 滚控制面到 v0.8 ✅

清单 tag `v0.7 → v0.8`，然后：

```
### MATCH: the running container is the build that was pushed
tai-talea v0.8 (93063d8, 2026-10-14T18:55:21Z, go1.27.1)
{"msg":"starting","mode":"fixed","gear":"1:1","capacity_actions":false,"provision_timeout":"45m0s"}
```

`imageID` 与 `sha256:e00530bd…` **逐字一致**，`--version` 自证。**暴露窗口结束**——
从此起任何新建的 worker Pod 都会被要求答 `/2`。

### 7.3 重建两台 worker Pod ✅（但路径换了）

原计划是「删 Pod + **用同样的名字**重新 provision」，理由是「实例身份就是 Pod 名，而 LOST→可达
本来就会走一次 `Prepare()`」。**这条路实测走不通**：

- 删掉 Pod 后，pull 把「Pod 不见了」读成**容量被回收** ⇒
  `beginDrain(reason="capacity revoked by partner")` → `PendingRelease` → drain 截止后
  `instance released`（`absent_from_partner: false`，即**真的把 Pod 也删了**）；
- 与此同时 `runSelfProvision` 已经建好了新 Pod（同名），但 `EnrollPrepared` 的
  `transition(ExpectInstanceState=Allocating)` 撞上 Released 行 ⇒
  `ERROR enrolment failed … "instance state changed concurrently"`；
- 而且 `provisionJobs` 的 FAILED 记录在**内存**里，所以同进程、同名再 provision 永远回
  **409「实例名已在创建中」**——这条路只能靠滚控制面或换名解开。

**实际采用的是 D1 验证过的那条路：换新名字。** 旧行此时已是 `RELEASED`，所以它对新实例的
回收被忽略；新名字建行走 `EnrollPrepared` 的 `else` 分支，干净。

```
talea-cmcc-k8s-1014-193606      → container enrolled endpoint=http://10.214.81.70:9001  instance prepared
talea-cmcc-k8s-1014-193606-1    → container enrolled endpoint=http://10.214.81.67:9001  instance prepared
```

**两台都 `instance prepared`** 是双向契约的判据：v0.8 期望 `/2`、v0.3 答 `/2`，于是 `Prepare()`
里的 `manager.go:130` 不再报 `bootstrap_version_mismatch`。角色分派与旧一对一致
（`…193606` = prefill、`…193606-1` = decode）。

> 机制上值得记一笔：两台都是 hostNetwork Pod，**Pod IP 就是节点 IP**，所以「重建」并不会换端点。
> 这也解释了为什么 decode 能在**同一个 URL** 上重新注册成功——而 `RegisterWorker` 对已存在的
> URL 会返回 `ErrAlreadyRegistered`，`readinessOnce`（`lifecycle.go:571-603`）因此必须显式
> `SetReadiness(true)` 把上一轮 drain 关掉的就绪重新打开。这一步按设计走通了。

### 7.4 验收（部分）

**探针确实进了 Pod spec**（两台都是）：

```
image:      …/library/talea-worker:v0.3
readiness:  /bootstrap/health    10 x 6   hdr=['X-Bootstrap-Token']
liveness:   /bootstrap/liveness  60 10 x 6 hdr=['X-Bootstrap-Token']
terminationGracePeriodSeconds: 300
phase=Running  ip=10.214.81.70 / 10.214.81.67  restarts=0
```

- **`restarts=0` 且不增长** ⇒ 探针通过，没有把正常的慢启动误判成死（§3.1 的时序是对的）。
- **decode 已回来**：`talea-cmcc-k8s-1014-193606-1  state=IDLE svc=SERVING role=decode attempts=0
  worker=6706184e-…`，Router 侧 `http://10.214.81.67:9002 type=decode healthy=True load=0`。
- **prefill 未成**：`talea-cmcc-k8s-1014-193606 state=IDLE svc=FAILED role=prefill attempts=3`，
  Router 侧 `type=prefill healthy=False`，`/readiness → 503 insufficient healthy workers`。
  原因是**外部资源**，不是我们的代码——见 §11。
- 于是 Router 的 `/readiness` 是 503、1P1D 的端到端推理**没做**。

### 7.5 原计划的四步（保留，供复用时对照）

1. **提升候选配置**（`stage-config-v07.py promote`，带 `TALEA_CFG_BACKUP=…`）。
   此刻 live 配置变成「image v0.3 + 探针开」，而 v0.7 控制面**仍在内存里用旧配置**——
   这正是必须紧接着第 2 步的原因：这段时间里 v0.7 若重启会读到新配置，而它期望 `/1`，
   新建的 v0.3 Pod 会被判死。
2. **滚控制面到 v0.8**：先把 `deploy/kubernetes/tai-talea-control-plane.yaml` 的 tag 改成 `v0.8`，
   再 `ns-deploy-cp.py <manifest> --digest sha256:e00530bd…`，然后 `ns-version.py` 让镜像自证。
   **暴露窗口从这一刻开始。**
3. **重建两台 worker Pod**：v0.3 大层已在节点 content store，只拉 19 KB 新层，所以 Pod 重建本身是秒级；
   贵的是每台约 7 分钟的 8 卡权重重载。
4. **验收**：Pod spec 里 `livenessProbe` 存在且指向 `/bootstrap/liveness`；
   kubelet 探针 200 且 `restartCount` 不增长；实例回到 `SERVING`；1P1D 真实推理 200。

**第 3 步的教训**：删 Pod 之后 pull 会把它读成容量回收。所以「重建」这个动作在这个平台上
**等价于「释放 + 换名重新纳管」**，而不是「原地重启」。

---

## 8. Rollback

| 要回退什么 | 怎么退 | 注意 |
| --- | --- | --- |
| 控制面二进制 | 清单 tag 改回 `v0.7` + `ns-deploy-cp.py --digest sha256:cd60180c3711…` | 见下一行：镜像与配置不能分开退 |
| 线上配置 | 用 `tai-talea.yaml.bak-before-v08`（= v0.7 那版 `a84de79f…`）覆盖 live | 本机也留了 `cfg-live.yaml`，sha256 相同 |
| worker 镜像 | 配置里 image tag 改回 `v0.2` + 重建 Pod | v0.2 的大层仍在 registry 与节点上 |
| 两者必须一起 | v0.7 二进制期望 `/1`，v0.3 镜像答 `/2`。**只退 tag 会得到一个永远起不来的 worker** | 这是 `/2` 的全部意义 |

`CGO_ENABLED=0` 静态二进制，回滚不需要重编译。

---

## 9. 仍然没做 / 欠债

- **1P1D 端到端推理没做**：prefill 缺卡（§11）。
- **B 的「冻住⇒重启」这条主张本身不依赖 GPU**，所以在窗口里单独证了（§10）——
  不必等卡。
- **A1 仍未被真实触发**：窗口里 prefill 是 `attempts=3` 的正常烧完 + park，
  不是 A1 的重臂路径。`rearm=0`。
- **镜像里的 `/opt/tai-talea/profile.json` 仍写 `bootstrap_version: "tai-talea-bootstrap/1"`**。
  本次**刻意没有覆盖它**：它只被携带、从不被比较（§1），而覆盖一个读不到内容的文件是净风险。
  §3 的探针输出把这个事实明明白白打了出来。下一次重打整盘镜像时它会自然跟上。
- **`start_parked_at` 零值序列化成 `"0001-01-01T00:00:00Z"`**（`omitempty` 对 `time.Time` 无效），纯外观，仍在。
- **增量层的新义务**：v0.3 的 manifest 引用 v0.2 的 12 GiB blob。若 registry 侧 GC 掉那个 blob，
  v0.3 会在某台机器重新拉取时失效——构建脚本已把「HEAD 该 blob 必须 200」写成硬断言（§7.0 已跑过）。

---

## 10. B 的核心主张：冻住的进程树会被 kubelet 重启（真机独立证明）

§7.4 只证明了**负的那一半**：探针不误杀。**正的那一半**——「树冻住时探针失败、kubelet 重启它」——
才是 B 存在的理由，因为 NVRM 卡死留下的正是**活着但哑**的进程树：没有任何东西会退出，
所以没有 k8s 的介入它永远不会被重启。

**为什么不能拿在跑的 worker 验**：冻住一个线上 worker 需要 `kubectl exec`，而 RBAC 拒绝。
所以用一个**一次性 Pod**（同镜像、**不申请 GPU**、自己的 netns、不加载模型），让它**冻住自己**：

- `SIGSTOP` bootstrap 进程 ⇒ liveness 端点停止应答（= NVRM 卡死的可观测形状）；
- `SIGSTOP` PID 1 ⇒ 容器**忽略 SIGTERM**，只有不可捕获的 SIGKILL 能结束它。

最后一条是判据的关键：如果 `lastState.terminated.exitCode` 回 `0` 或 `143`，那就只是一次礼貌的
终止，实验**并没有**复现卡死；必须是 **137**。

时序按比例收紧（`10s / 3s ×2 / grace 10s`，线上是 `60s / 10s ×6 / grace 300s`），
kubelet 走的机制完全一样。脚本：`ns-liveness-kill.py`，跑在 h100-01。

```
### imageID …/library/talea-worker@sha256:e8eba9ed91e2385e65a6f0cad97fe71cecaa19f5e1bf92e1b9bea7735afe9e1d
### restartCount=0  startedAt=2026-10-14T19:56:44Z
### restartCount=1  lastState.terminated={"exitCode": 137, "reason": "Error",
                     "startedAt": "2026-10-14T19:56:44Z", "finishedAt": "2026-10-14T19:58:02Z"}
### restartCount=2  lastState.terminated={"exitCode": 137, "reason": "Error",
                     "startedAt": "2026-10-14T19:58:02Z", "finishedAt": "2026-10-14T19:59:20Z"}

--- 容器自己的日志（bootstrap 的 log_file）---
{"ts":"2026-10-14 19:58:53,089","msg":10.214.81.67 - "GET /bootstrap/liveness HTTP/1.1" 200 -}
{"ts":"2026-10-14 19:58:56,089","msg":10.214.81.67 - "GET /bootstrap/liveness HTTP/1.1" 200 -}
{"ts":"2026-10-14 19:58:59,089","msg":10.214.81.67 - "GET /bootstrap/liveness HTTP/1.1" 200 -}

=== WEDGE: freeze the tree. alive, but mute ===
  SIGSTOP 14  <- /opt/tai-talea/venv/bin/python -m tai_talea_bootstrap serve --profile … --port 9001 …
  SIGSTOP 9   <- bash /opt/tai-talea/entrypoint.sh serve
  SIGSTOP 1   <- the container init itself
```

四件事同时被证：

1. `imageID` 与推送的 manifest digest **逐字一致** ⇒ 验的是 v0.3 这个镜像，不是某个 tag。
2. **kubelet 的探针真的在打这个端点，并且带对了 token**：容器自己的请求日志里，每 3 秒一条
   `10.214.81.67 - "GET /bootstrap/liveness" 200`——来源 IP 就是节点 IP，节奏等于
   `periodSeconds`。这一条同时证明 §3 里那个「探针必须带 `X-Bootstrap-Token`，否则 401」
   在**真实 kubelet 上**是配好的。
3. 冻住之后 `restartCount 0→1→2`、每次 `exitCode 137`、`reason Error` ⇒
   **kubelet 杀掉了它，而且用的是 SIGKILL**（SIGTERM 对冻住的树无效）。
4. 重启是**同 Pod 名、同 IP、同租约**的原地复活——正是 §5 里想要的形态
   （「等价于人工杀 PID 1」，只是由 kubelet 自己干）。

---

## 11. 尚未闭环：prefill 缺卡（外部原因），以及怎么接回去

**现象**：`talea-cmcc-k8s-1014-193606 state=IDLE svc=FAILED role=prefill attempts=3
rearm=0 parked=2026-10-14T19:50:30Z`；Router `type=prefill healthy=False`；
`/readiness → 503 insufficient healthy workers`。占位的是 **h100-04**。

**取证（只读）**：`ns-gpu-holders.py`（`--host --hostpid`）显示 h100-04 的 8 张卡全部被
**第三方 `docker run` 的容器**占住，每卡约 71 GB：

```
cgroup: /system.slice/docker-0ad4bbd3…scope   (h100-night-decode-20261014,  19:44:14Z 起)
cgroup: /system.slice/docker-013045ed…scope   (h100-night-encoder-20261014, 19:44:13Z 起)
argv:   sglang::scheduler_DP{0..3}_TP{0..7}_EP{0..7}      ← 8 个进程，正在加载同一个 GLM-5.3-Flash
镜像:   vllmascend-…cmecloud.cn/dev/sglang:0.5.21-glm53-release-1014
标签:   task=h100-night-20261014 / cmss.task=glm53-realtune-mcl / cmss.release=q8fp8-fused-store-20261014
```

**不是我们的 bug**：prefill 的 SGLang 被 `-9` 杀掉（`sglang exited unexpectedly with code -9`），
因为 8 张卡上没有余量。控制面按设计走了正常的重试预算（`attempts=3`），
`detail_exhausted:"false"`。对比 h100-01：8 卡全是**我们自己的** `/kubepods/...` decode worker，
所以只有 prefill 一侧受影响。

**第三方没有可等的 deadline guard**：`deadline_guard.py` 只 `docker stop label=h100-night-20261014`
（碰不到 k8s/containerd），`/tmp`、`/root`、`/data03` 下也**没有** `*guard*` / `*deadline*` 残留
（`ns-third-party-window.py` 查过）。所以只能等它自己结束。

**卡释放后怎么接回去**（这就是 `requeue` 的用途）：

```
POST /v1/capacity/console/instances/talea-cmcc-k8s-1014-193606/requeue
```

`RequeueService`（`internal/controller/lifecycle.go:742-797`）会**把整个重试预算复位**：
`StartAttempts=0` + `StartRearms=0` + 清 `StartParkedAt` + 把 OpStart 重写成 `OpPending`。
前置条件只有三条，此刻都满足：实例必须是 `IDLE/FAILED`、**预算已烧完**
（没烧完它会拒绝并提示「reconciler retries it on its own」）、且有已分配的角色。

> 也就是说：**A1 把 `start_retry_max_rearms` 用尽的实例永远可救，不需要滚控制面**
> （滚控制面反而更差：实例表是持久化的，`rearms` 会跟着走）。
>
> 若 A1 的首次重臂（park 后 30m）在卡仍被占时白跑一次，结果是 attempts 又烧完、再次 park——
> 此时 requeue 依然有效。所以不必抢在重臂之前干预。

**为什么不现在 release**：`BeginRelease` 要求 `ServiceState==None`，而 prefill 是 `ServiceFailed`
（`HasService()` 为真）⇒ 得先 drain，而 drain 一个已 FAILED 的服务没有意义。等卡释放后 requeue
是代价最小、且不丢实例身份的路。
