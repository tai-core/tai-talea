# 中国移动 nightshift 平台 · 工具使用说明书

> 适用版本：控制面 `tai-talea v0.8`（`93063d8`）+ worker 镜像 `talea-worker v0.3`
> 编写时间：2026-10-15 · 覆盖 D1 验收、v0.7 上线、v0.8+B 上线三轮实测
>
> 本文是**操作手册**：怎么连、怎么看、怎么下单、怎么救、什么时候不能动。
> 设计理由与历史事故放在 `docs/` 的其余文档里，本文只在需要判断时引用。

---

## 0. 这个工具是什么

一句话：**把合作方给的 GPU 容器，变成一对能对外服务的 prefill/decode 推理副本，并在这个过程中自己做健康检查、角色分配和故障回收。**

它由两部分组成，缺一不可：

| 部分 | 是什么 | 在哪 |
| --- | --- | --- |
| **控制面** | 一个 Go 二进制，是唯一的决策者 | Pod `tai-talea`，`hostNetwork`，`0.0.0.0:8787` |
| **worker 镜像** | 一台机器上的一整套运行时（bootstrap + SGLang + 权重） | 每个 worker Pod，由控制面创建 |

**它能做的**：建容器、装、跑、注册到 Router、按角色分配、看负载、按需增减、故障重试、把冻死的 worker 交给 kubelet 重启。
**它不能做的**：替你抢别人正在用的卡；替你和第三方协调；把 1 台机器变成 2 台。

---

## 1. 五分钟上手

```bash
# 0. 约定：所有命令都通过"跳板机 n1 + 一次性探针 Pod"执行，见 §2
cd <本地工具目录>            # 本仓库之外，内含 nsrun.py / nrsh.py 等
PY="C:/Users/.../envs/default/Scripts/python.exe"   # 只有这个 python 装了 paramiko

# 1. 现在是什么状态？
$PY nsrun.py ns-state-v07.py --ctrl-pod tai-talea --host

# 2. 要机器：申请两台（名字一次性，见 §4.5）
$PY nsrun.py ns-provision-two.py --ctrl-pod tai-talea --host      # NAMES=... PARTNER=cmcc-k8s

# 3. 验收：Router 就绪 + 真发一次推理
$PY nsrun.py ns-probe-status.py --ctrl-pod tai-talea --host
```

判据只有三条：`/readiness` 回 **200**、两个实例都是 `svc=SERVING`、真实推理回 **200**。

---

## 2. 系统在哪、怎么连

| 项 | 值 |
| --- | --- |
| 集群 apiserver | `192.168.203.2:6443` |
| 命名空间 | `nightshift` |
| 节点 | `h100-01` = `10.214.81.67`、`h100-04` = `10.214.81.70`，各 8×H100 |
| 控制面 Pod | `tai-talea`（hostNetwork 钉在 h100-04）→ `10.214.81.70:8787` |
| Router Pod | `talea-router`（hostNetwork 钉在 h100-04）→ `10.214.81.70:8000` |
| worker 端口 | bootstrap `9001`、SGLang `9002`（都挂在宿主 netns 上） |

三条必须记住的访问约束：

1. **跳板机 `n1` 只到得了 apiserver**，到不了 `10.222.x`（Pod 网段）和 `10.214.x`（节点网段）。
2. **控制面没有 Service、没有 NodePort、没有 Ingress**，而且这个租户**被禁止 port-forward**（RBAC 极窄）。
   ⇒ **所有 HTTP 请求都必须在集群内部发出**，也就是在探针 Pod 里发。
3. 控制面 Pod **开了 hostNetwork**，所以它每次滚动都会换集群 IP。
   ⇒ **长驻观察者跨不过滚动**（它的 `CTRL` 在启动时解析一次），滚动后必须重启它。

因此本地只做两件事：`ssh n1` 下发 YAML，用探针 Pod 读结果。

```
nsrun.py <driver.py> [--host] [--hostpid] [--hostnet] [--ctrl-pod tai-talea] [--env K=V]
    apply 一个一次性 Pod → 在里面跑 <driver.py> → 打印它的 stdout → 删掉它
    --ctrl-pod 会自动把 CTRL=http://<控制面 Pod 的当前 IP>:8787 注入成环境变量
nrsh.py <script.sh>
    把脚本上传到 n1 执行（用于 kubectl / 构建；不要用 nsk.py，它会吃掉引号）
```

**探针镜像里没有 nvidia-smi** ⇒ 要看卡必须 `chroot /host nvidia-smi`。

---

## 3. 对象模型（先懂这个，后面全是推论）

工具管理的最小单位是 **Instance（实例）= 一台容器**。实例有两个**互相独立**的状态轴：

```
InstanceState（容器本身）        ServiceState（容器里跑的 SGLang）
  ALLOCATING   正在铺装            NONE        没跑
  PREPARING    在准备（装/自检）    STARTING    正在拉起
  IDLE         容器就绪、空闲       HEALTHY     起来了但还没在 Router 上就绪
  RELEASING    正在归还             REGISTERING 正在注册到 Router
  RELEASED     已归还（行保留）     SERVING     在服务 ✅
  LOST         探不到容器           DRAINING    正在摘流
                                   FAILED      启动失败（可重试/可复位）
```

外加一个 `Role`：`prefill` / `decode` / 空。
**合法性**由 `CheckCombination` 判定，`GET /v1/capacity/console/instances/{id}` 会把 verdict 一起返回。

三条容易搞错的语义：

- **`IDLE` 只说明容器在，不说明它闲着** —— 空闲与否看 `ServiceState`。
- **`RELEASED` 的行不会被删**，名字还占着。实例名**一次性**（见 §4.5）。
- **角色只在 `SERVING` 前后才有意义**；`Role` 为空时这台机器归 planner 调遣。

一个可用的 1P1D 服务长这样：

```
talea-cmcc-k8s-1014-193606      state=IDLE  svc=SERVING  role=prefill
talea-cmcc-k8s-1014-193606-1    state=IDLE  svc=SERVING  role=decode
```

---

## 4. 日常操作速查

鉴权一律 `Authorization: Bearer <admin_token>`，`admin_token` 在线上配置里
（节点侧 `/data/tai-talea/config/tai-talea.yaml`）。下面路径都相对 `http://<控制面>:8787`。

### 4.1 看状态

| 意图 | 方法 |
| --- | --- |
| 全部实例 | `GET /v1/capacity/console/instances` |
| 单个实例 + 合法性判定 | `GET /v1/capacity/console/instances/{id}` |
| 负载与齿轮 | `GET /v1/capacity/console/telemetry` |
| 计划器 | `GET /v1/capacity/planner` |
| 某实例的审计流水 | `GET /v1/capacity/console/instances/{id}/audit` |
| 近期事件 | `GET /v1/capacity/events` |

`telemetry` 是**唯一的负载来源**。判可用性看每台 worker 的
`Running / Queue / MaxRunning / Prealloc / Transfer` **五个字段是否都非 nil**：
缺任何一个 ⇒ `Complete=false` ⇒ **负载输入被静默关闭**（不是报错，是安静地不参与决策）。
若看到 `指标不可用（请检查 --enable-metrics）`，说明该 worker 的 SGLang 没起来。

### 4.2 申请机器（下单）

```
POST /v1/capacity/console/provision
{"partner_id":"cmcc-k8s","name":"talea-cmcc-k8s-1015-0430","count":2}
→ 202 {"accepted":true,"job":{...}}
```

- `count` 上限 **4**（超过回 400）。
- 名字冲突回 **409「实例名已在创建中」** —— 注意这条**含内存态**：即使实例已经
  `RELEASED`，只要同一个控制面进程里还留着那次失败的 job，这个名字就永远下不了单，
  只能**换一个名字**（§4.5）。
- 下单是**异步**的：`GET /v1/capacity/console/provision` 看队列。

**执行顺序是自动的**：建容器 → 铺装 → 自检（`Prepare`）→ 分配角色 → 起服务 → 注册到 Router → 就绪。
全程不需要人工干预，这就是"自动化走完"的部分。

### 4.3 复位重试预算（最常用的救火动作）

```
POST /v1/capacity/console/instances/{id}/requeue
```

把**整个重试预算**清零：`StartAttempts=0` + `StartRearms=0` + 清掉 park 时间 + 把启动操作重写成待执行。
下一步 reconcile（默认 10s 一轮）就会重试它被分配的角色。

**前置条件**（不满足会 409 并说明原因）：
1. 实例必须是 `IDLE/FAILED`；
2. 预算必须**已经烧完**（还有余额时会回「reconciler retries it on its own」）；
3. 必须有已分配的角色。

> 记住这条：**一台把自动重试（含 A1 重臂）用完的机器，永远能用 `requeue` 救回来，
> 不需要滚控制面。** 滚控制面反而更差——实例表是持久化的，`rearms` 会跟着走。

### 4.4 摘流 / 归还

| 意图 | 方法 | 前置 |
| --- | --- | --- |
| 从 Router 摘下来（不改角色） | `POST /v1/capacity/instances/{id}/drain` | — |
| 把机器还给合作方 | `POST /v1/capacity/instances/{id}/release` | **`ServiceState` 必须是 `NONE`** |
| 回收（同 release 的语义，另一条入口） | `POST /v1/capacity/instances/{id}/reclaim` | 同上 |

- **`release` 不能直接对 `FAILED` 用**：`FAILED` 也算"占着服务"（`HasService()` 为真），
  会回「drain it first」。要么先 drain，要么走 requeue 把它救活。
- 归还之后**节点会永久留在 Router 的注册表里**（`is_healthy:false`）。
  这是**无害的**：选路会跳过它。但别把它当故障排查。
- 归还**不会删 Pod**（在某些路径下会，见 §4.5）。

### 4.5 ⛔ 实例名一次性 —— 这条最容易踩

**删掉 worker Pod 之后，不要用同一个名字重新下单。** 实测会这样：

1. pull 把"Pod 不见了"读成**容量被回收** ⇒ 自动 `beginDrain(理由: capacity revoked by partner)` → `beginRelease`
   → 最终 `instance released`（**真的把 Pod 也删了**）；
2. 与此同时下单逻辑已经建好了同名 Pod，但铺装步骤撞上那行 `RELEASED` ⇒
   `ERROR enrolment failed … "instance state changed concurrently"`；
3. 而且失败的 job 记在**内存**里 ⇒ 同名再下单永远 **409**，只能换名或滚控制面。

✅ **要重建一台 worker，正确做法是：删 Pod + 用新名字下单。**

### 4.6 强制对账 / 计划器

| 意图 | 方法 |
| --- | --- |
| 立刻跑一轮对账（不等 10s） | `POST /v1/capacity/reconcile` |
| 改计划器 | `PUT /v1/capacity/planner` |
| 看计划器 | `GET /v1/capacity/planner` |

⚠️ **读配置别被 `mode:` 骗**：`mode: readiness-v1` 那个键属于 **`router:` 段**；
`planner:` 段里**没有** `mode` 键 ⇒ 取默认 `fixed`，也没有 `capacity_actions` 键 ⇒ 默认 `false`。
也就是说"齿轮钉死 1:1、不自动增减容量"是由**缺席**表达的，不是由显式 `false` 表达的。

---

## 5. 一次完整的"上线"长什么样

以 D1 验收（`docs/d1-acceptance-2026-10-11.md`）为准，**14 分 20 秒**跑完，全程无人工介入：

| 阶段 | 判据（日志里长什么样） |
| --- | --- |
| 建容器 | `container enrolled endpoint=http://10.214.81.70:9001` |
| 铺装+自检 | `instance prepared`（两台都出现才算过；失败会写 `reason="bootstrap_version_mismatch"` 一类） |
| 分配角色 | `planner round applied summary="gear=1:1 target=1/1 serving=…"` |
| 起服务 | `instance started role=prefill` / `role=decode` |
| 注册 Router | Router `GET /workers` 里出现两条，`healthy=true` |
| 就绪 | Router `GET /readiness → 200 {"status":"ready","healthy_workers":2,…}` |
| 验收 | 真发一次 `POST /v1/chat/completions`（或 `/generate`）回 **200** |

**只有最后一行是"真的能用"的判据**，前面全部是过程指标。

---

## 6. 自动化能力现状

| 层 | 状态 |
| --- | --- |
| ① 容器 → 在服务 | ✅ 已闭环（D1） |
| ② 裸容器 → 就绪 | 代码在，九章平台未跑通 |
| ③ 自动增减容量 | 代码在，**两个平台都关着**（`capacity_actions: false`） |
| ④ 自愈 | **A 已上线**（重试预算 / 重臂 / requeue）；**B 已上线**（liveness 探针，kubelet 自己重启冻死的进程树） |

**结论：部署这条链是自动的，运维这条链还不是**——③ 关着，② 只有一个平台通。

### B 到底解决了什么（值得单独记）

NVRM 卡死时，worker 的进程树**活着但不再应答**：没有东西会退出，所以 k8s 原本永远不会动它（实测放置 10 小时）。
v0.8/v0.3 之后，worker Pod 里有一个**不持锁、不碰模型、不碰磁盘**的探针端点
`/bootstrap/liveness`，kubelet 每 10s 打一次；连续 6 次失败（= 至少 60s 宽限后）就用 **SIGKILL** 结束它，
容器**同 Pod 名、同 IP、同租约**原地重启。

真机实测（一次性 Pod，无需 GPU）：冻住进程树后 `restartCount 0→1→2`、每次 `exitCode **137**`（137 = SIGKILL，
证明 SIGTERM 对冻住的树无效）。详见 `docs/talea-worker-v03-and-the-liveness-contract-2026-10-15.md` §10。

---

## 7. 排障手册

| 现象 | 判据 | 动作 |
| --- | --- | --- |
| 实例 `svc=FAILED attempts=3` | 看 sglang 的日志文件（**容器内** `/opt/tai-talea/logs/sglang-*.log`；bootstrap Pod 日志只有 HTTP 行） | 若是 `-9`/OOM：**先确认卡有没有被别人占**，再 `requeue` |
| 权重加载中途冻住 | 日志停在 `Loading safetensors … N%`，进程不退、`dmesg` 刷 `NVRM: knvlinkSendInbandData_IMPL` | **v0.3 起不用管**：等 kubelet 自己 SIGKILL 重启 |
| Pod `0/1 Running` 但实例 `FAILED` | Pod 调度成功 ≠ 有卡用 | 卡被别人占了。这是**资源**问题，不是代码问题 |
| Router `/readiness` 503 | `insufficient healthy workers` | 数 `GET /workers` 里 `healthy=true` 的条数；<2 就一定 503 |
| 新 worker 一直不 healthy | Router 默认 60s×2 才判健康，而控制面只等 25s | 确认 Router 带了 `--health-check-interval-secs=5 --timeout-secs=3 --success-threshold=1` |
| 实例 `LOST` | 探针一次不可达即判（**没有连续失败门限**） | 只要它变得可达就会被强制重新 `Prepare()`；观察即可 |
| 摘流卡在 `DRAINING` | LOST 的容器不能调 `bootstrap/stop`（端点不答） | 设计上 LOST 直接记 stopped；若卡住，`requeue` 或重建 |
| 删 worker 的 prefill 永远失败 | Router 的 `AmbiguousPrefillTransfer` 是**只作用于 prefill** 的黏性闩锁 | 绕开：原地重启 Router（代价：成员表清空、id 全换、队列丢弃） |
| 同名下单 409 | 内存态 job 残留 | 换名字（§4.5） |
| 探针打了 401 | 少了 `X-Bootstrap-Token` 头 | 探针配置里必须带（kubelet 会把 401 读作**失败**） |
| 一个实例都起不来，`503 No available prefill workers` | 只有 1 台 = 零服务，0P1D **不是降级** | 必须凑齐 1P + 1D |

---

## 8. 变更窗口（滚控制面 / 改配置 / 重建 worker）

**顺序有硬约束，不能拆开做。** 理由：探针写在 Pod spec 里、创建时固化，所以
"打开探针"对**已经存在的 Pod 完全无效**；而"控制面期望的契约版本"一变，旧容器会被判死且**自愈不了**。

正确顺序：

```
1. 写候选配置 tai-talea.next.yaml（不碰 live）
2. 用新控制面镜像 --check-config 预检候选配置        ← 通过才继续
3. promote：cp 候选 → live（先留备份）
4. 滚控制面：改清单 tag → ns-deploy-cp.py --digest <新 digest>
   · 必须比对运行容器的 imageID 与 digest **逐字**（tag 不是证据）
5. 重建全部 worker Pod（用**新名字**，§4.5）
6. 验收：探针在 spec 里 / restartCount 不增长 / 实例 SERVING / 真推理 200
```

⚠️ 第 4 步单独做**收益为零、代价非零**：收益是零（探针不会进已存在的 Pod），
代价是"期望 `/2` 而 worker 还答 `/1`"期间一次探针抖动就烧完预算并卡死。
⛔ 第 5 步**如果中途失败**，控制面处于无人状态，立刻用正确路径重跑。

---

## 9. 回滚

| 回退什么 | 怎么退 | 注意 |
| --- | --- | --- |
| 控制面二进制 | 清单 tag 改回 `v0.7` + `ns-deploy-cp.py --digest sha256:cd60180c3711…` | **必须和配置一起退** |
| 线上配置 | 用节点侧备份 `tai-talea.yaml.bak-before-v08`（= `a84de79f…`）覆盖 live | 备份就在配置同目录 |
| worker 镜像 | 配置里 image tag 改回 `v0.2` + 重建 Pod | v0.2 的大层仍在 registry 与节点上 |

⚠️ **镜像与配置必须一起回退**：旧二进制不认识 `start_retry_*` 这类新键，只退 tag 会启动失败；
`/1` 与 `/2` 的契约同理。`CGO_ENABLED=0` 静态二进制，回滚不需要重编译。

---

## 10. 已知限制与欠债

- **一台 worker = 整个节点**（`gpu_count: 8`）。所以 1P1D 需要 2 个节点；只有 1 台等于零服务。
- **节点上还有别人**。卡是否可用要**实测**，不能看 k8s：宿主 `docker run` 起的容器**不占 k8s 的 GPU 配额**，
  所以 k8s 会痛快地把 8 张卡"分配"给我们的 Pod，然后 SGLang 在启动时饿死。
  **判归属只看 cgroup**：`/kubepods/...` 是我们的，`/system.slice/docker-<id>.scope` 是别人的。
- **平台不提供容量/队列查询**，所以边界只能自己设（`provision_timeout` 6h + `wait_timeout` 5h30m，
  且后者必须更小）。
- **排队期不计费也不占位**：QUEUED/PENDING/SCHEDULING 期间重试预算停表，所以不要在排队期靠重试推进。
- **`bootstrap_version` 是编译期常量**，不在配置文件里，`--check-config` 也抓不到；升它只能改 Go + 重滚控制面。
- 外观欠债：`start_parked_at` 零值序列化成 `"0001-01-01T00:00:00Z"`；镜像内
  `/opt/tai-talea/profile.json` 仍写 `bootstrap_version: /1`（**不被比较**，刻意未覆盖）。
- 增量层义务：`talea-worker:v0.3` 的 manifest 引用 `v0.2` 的 12 GiB layer。
  registry 侧若 GC 掉那个 blob，v0.3 会在某台机器重新拉取时失效（构建脚本已把它做成硬断言）。
- **对外不可达**：n1 到不了推理地址，且租户被禁 port-forward、没有 Service/Ingress。
  要让外面真的用上，需要放行 port-forward，或加 NodePort/LB/Ingress，或加代理。

---

## 11. 工具清单

| 工具 | 用途 |
| --- | --- |
| `nsrun.py <driver.py>` | 在探针 Pod 里跑一段 Python，读集群内的状态 |
| `nrsh.py <script.sh>` | 在跳板机上跑脚本（`kubectl`、构建） |
| `ns-state-v07.py` | 一屏看完：Pod / Router / 实例 / telemetry |
| `ns-provision-two.py` | 按名字下单（`NAMES` / `PARTNER` 环境变量） |
| `ns-delete-workers.py <name...>` | 删 worker Pod 并**等它真的消失** |
| `ns-gpu-holders.py` | 卡上到底是谁：cgroup + 起止时间 + 第三方容器 |
| `ns-worker-probe.py` | 用一次性 Pod 验 worker 镜像的端点（200/401/404） |
| `ns-liveness-kill.py` | 验"冻住的树会被 kubelet 重启"（不需要 GPU） |
| `ns-deploy-cp.py` / `ns-version.py` | 滚控制面 / 让镜像自证版本 |
| `ns-revive-router.py` | 原地重启 Router（复位 id 与队列） |

---

## 12. 一条底线

**不要动别人的容器。** 节点上的第三方作业（`h100-night-*`、`glm-chy`、`eval-chy`）
在 2026-10-15 实测是**正在服务的实验**：`h100-night-router-20261014` 当时正在连续返回
`POST /v1/chat/completions 200`，并且有它自己的截止守卫 `deadline_guard.py` 在跑。
需要卡的时候，正确的做法是**等它自己结束**，或者**去协调**，不是去 stop 它。

同理，本仓库的红线目录（第三方工作）**禁止访问、挂载或引用**。
