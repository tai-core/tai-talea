# worker v0.3 与 liveness 契约：B 的镜像面就绪，上线等窗口（2026-10-15）

> **状态**：worker 镜像 v0.3 已推送并在真机上验透；控制面 v0.8 已构建、已推送、已用候选配置预检通过。
> **但控制面没有滚、worker Pod 没有重建、线上配置没有提升。** 现网仍是 v0.7 + `talea-worker:v0.2`。
> §5 记录为什么必须停在这里——这不是拖延，是 `/2` 与「先不重建 Pod」这两件事互相抵消。

---

## 0. 结论速览

| 项 | 状态 | 证据 |
| --- | --- | --- |
| worker 镜像 `talea-worker:v0.3` | ✅ 已推送 | manifest `sha256:e8eba9ed…5afe9e1d` |
| `v0.3` 携带 `/bootstrap/liveness` | ✅ 真机验透 | §3：200 / 401 / 404 三态 |
| 控制面二进制常量 → `/2` | ✅ 已改已提交 | `93063d8`，`cmd/tai-talea/main.go:267` |
| 控制面镜像 `tai-talea:v0.8` | ✅ 已推送 | manifest `sha256:e00530bd…8c69195e` |
| 候选配置（v0.3 + 探针开） | ✅ 已 stage 在节点侧路径 | `tai-talea.next.yaml` `sha256:e7b0e974…` |
| v0.8 对候选配置的预检 | ✅ exit 0 | §4 |
| **滚控制面 / 提升配置 / 重建 worker Pod** | ❌ **未做（刻意）** | §5、§7 |
| 现网是否受影响 | ✅ 未受影响 | §6 |

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

## 7. 待执行的窗口

预备状态（已完成，可直接用）：

- worker 镜像 `v0.3`：已推送、真机验透。
- 控制面镜像 `v0.8`：已推送、对候选配置预检 exit 0。
- 候选配置：已在节点 `/data/tai-talea/config/tai-talea.next.yaml`（`e7b0e974…`）。
- 节点侧备份 `tai-talea.yaml.bak-before-v08` 已建。

窗口要做的四步，**顺序是有约束的**：

1. **提升候选配置**（`stage-config-v07.py promote`，带
   `TALEA_CFG_BACKUP=tai-talea.yaml.bak-before-v08`）。
   此刻 live 配置变成「image v0.3 + 探针开」，但 v0.7 控制面**仍在内存里用旧配置**——
   这正是必须紧接着第 2 步的原因：这段时间里 v0.7 若重启会读到新配置，而它期望 `/1`，
   新建的 v0.3 Pod 会被判死。
2. **滚控制面到 v0.8**：先把 `deploy/kubernetes/tai-talea-control-plane.yaml` 的 tag 改成 `v0.8`
   （v0.7 那次是随滚动一起提交的，本次刻意没改，所以它现在仍是 v0.7），
   再 `ns-deploy-cp.py <manifest> --digest sha256:e00530bd…`，然后 `ns-version.py` 让镜像自证。
   **暴露窗口从这一刻开始。**
3. **重建两台 worker Pod**（v0.3 大层已在节点 content store，只拉 19 KB 新层，所以 Pod 重建本身是秒级；
   贵的是每台约 7 分钟的 8 卡权重重载）。这一步让两个 worker 开始答 `/2`，暴露窗口结束。
   具体机制在执行前选定并验证——候选是「删 Pod + 用同样的名字重新 provision」，
   因为实例身份就是 Pod 名，而 LOST→可达 本来就会走一次 `Prepare()`。
4. **验收**：Pod spec 里 `livenessProbe` 存在且指向 `/bootstrap/liveness`；
   kubelet 探针 200 且 `restartCount` 不增长；实例回到 `SERVING`；1P1D 真实推理 200。

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

- **B 尚未在线上生效**：worker Pod 未重建 ⇒ 线上没有任何 livenessProbe。§7 就是那个窗口。
- **A1 仍未被真实触发**：只有单测证据，现网 `rearm=0`。
- **镜像里的 `/opt/tai-talea/profile.json` 仍写 `bootstrap_version: "tai-talea-bootstrap/1"`**。
  本次**刻意没有覆盖它**：它只被携带、从不被比较（§1），而覆盖一个读不到内容的文件是净风险。
  §3 的探针输出把这个事实明明白白打了出来（`"bootstrap_version": "tai-talea-bootstrap/1"` 而模块是 `/2`）。
  下一次重打整盘镜像时它会自然跟上，或者单独补一个小层。
- **`start_parked_at` 零值序列化成 `"0001-01-01T00:00:00Z"`**（`omitempty` 对 `time.Time` 无效），纯外观，仍在。
- **增量层的新义务**：v0.3 的 manifest 引用 v0.2 的 12 GiB blob。若 registry 侧 GC 掉那个 blob，
  v0.3 会在某台机器重新拉取时失效——构建脚本已把「HEAD 该 blob 必须 200」写成硬断言，
  但这是运行期的依赖，不是一次性检查。
