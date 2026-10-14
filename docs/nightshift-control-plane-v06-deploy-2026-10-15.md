# 控制面 v0.6 上线：三处真 bug 的修复与部署（2026-10-15）

> 承接 `nightshift-recovery-router-reset-2026-10-14.md`（用 Router 复位解开角色变更死结）。
> 那篇解释「为什么坏、怎么救」；本篇交付**修复**并把它**推上线**。
> 时间一律 **UTC**（CST = UTC+8；本篇正文的 16:5x UTC = 次日 00:5x CST）。

---

## 0. 结论速览

| # | 项 | 结果 |
| --- | --- | --- |
| 1 | 修复内容 | 三处真 bug：**①** 瞬时健康抖动被判死；**②** 角色恢复路径被 `call_timeout` 结构性截断；**③** 两个 SIGTERM handler 互相覆盖 |
| 2 | 回归 | `go build`/`go vet`/`gofmt` 全绿；`go test ./... -count=1` **全绿**（controller 88.3s）；`python -m unittest` **73 项通过**（新增 6 条测试，Go 3 + Python 3） |
| 3 | 上线 | 控制面镜像 `…/library/tai-talea:v0.6`，`imageID` 与推送 manifest digest **逐字一致** |
| 4 | 自证 | 启动日志出现 `"unhealthy":0` —— 这是本轮**新增**的报告字段，等于「新代码在跑」 |
| 5 | 重启接管 | `checked=10 lost=0 recovered=0 reregistered=0`；两实例保持 `IDLE/SERVING`、`start_attempts=0`、**worker id 与滚动前逐字相同** |
| 6 | 对服务影响 | **零**：滚动窗口 Router 侧 `/readiness` 全程 200、`healthy_workers=2`；观察者记录 `T=0 Z=0`、`ALARM=0` |
| 7 | 验收 | D1 3/3 经 Router 拿到带 `usage` 的真实输出 |
| 8 | 仍未验证 | §5.2 那条路径（READY Router 上的 `settleService` → 重注册）v0.6 的 `removal_timeout` 修复**准备好了但没被真实触发过** |

---

## 1. 三处 bug 与它们的根因

### 1.1 bug①：一次探测失败 = 停服务 + 8 卡全量重载

`launcher/client.go:78` 的 `Healthy()` **只认 `Status == "ok"`**。
bootstrap 只要一次 `/health_generate` 失败就回 `degraded`
（HTTP **200**，body 里 `status: degraded`），`Healthy()` 不认。
而 `reconcile.go:317-323` 把这个瞬时事件直接当 `failStart` 处理：

```
healthErr != nil || !health.Healthy()  ->  failStart("running model is not healthy")
```

后果不成比例：**一次探测抖动 ⇒ 摘流 + `ServiceFailed` + 重走完整加载**。
在 8×H100 上这是一次几分钟的可用性事故，触发条件却只是一个 HTTP 请求没答上。

### 1.2 bug②：角色恢复路径被 `call_timeout` 结构性截断

`register()`（`lifecycle.go:418`）用 `c.callContext(ctx)` 包住
`cleaner.RemoveStaleMembership(...)`。而 `RemoveStaleMembership` 等的是 Router 的
**异步 `worker_removal` 工作流**，预算最多 **390s**；
`call_timeout` 线上是 **10s** ⇒ 这个调用**在结构上永远不可能成功返回**。

这不是"调小一点就好"的调优问题，是**用错了预算**：一个要等 390s 的等待，
被套进一个 10s 的壳里。`recovery doc §2.1` 里那 20 分钟的
`IDLE/REGISTERING ↔ IDLE/FAILED` 循环就是它的表现。

### 1.3 bug③：两个 SIGTERM handler，后者覆盖前者

`__main__.py` 与 `serve()` 都注册 SIGTERM handler，`serve()` 覆盖 `__main__.py` 的。
`__main__.py` 那个是对的（先停模型，再让 HTTP server drain），
被覆盖之后退出路径**不可控也不可观测** —— 这是上一轮「起来 46 秒就被拆掉」时
我们查不清退出原因的直接原因。

---

## 2. 改了什么（13 个文件，+358/−35）

### 2.1 bug①：把「摘流」和「判死」分开

新增 `controller.health_failure_threshold`（默认 **3**）。
`reconcile.go` 的判定改成先取连续失败计数：

- **每一轮失败都立刻摘流**（`closeReadiness`）—— 不健康就不该继续接流量；
- 但**只有连续失败达到阈值才判死**：

```go
streak := c.noteHealthFailure(instance.ID)
if err := c.closeReadiness(ctx, instance); err != nil { return "", err }
if streak < c.HealthFailureThreshold() {
    c.log().Warn("model health probe failed; instance taken out of rotation",
        "instance_id", instance.ID, "streak", streak, "threshold", c.HealthFailureThreshold())
    return "unhealthy", nil          // <- 新增动作，不改服务状态
}
return "", c.failStart(...)          // <- 只有到阈值才走到这里
```

探测恢复时 `c.clearHealthFailures(instance.ID)` 清零。
`ReconcileReport` 新增 `Unhealthy []string`，并为每个分支补上归集。

**这个设计的关键是「摘流」与「判死」不再绑定**：抖动期间流量不进来（对可用性有利），
但实例和服务状态不动（对成本有利）。

### 2.2 bug②：给这个等待一条自己的预算

新增 `router.removal_timeout`（默认 **8m**）与 `Controller.removalContext()`：

```go
// This is the one call that legitimately outlives call_timeout. …
// removal_timeout covers the workflow's budget instead.
cleanCtx, cleanCancel := c.removalContext(ctx)
err = cleaner.RemoveStaleMembership(cleanCtx, existing.ID, instance.ServiceURL(), role)
cleanCancel()
```

实例锁**全程持有** —— 所以并发操作会被拒绝而不是去和角色变更赛跑，
这正是想要的行为。

### 2.3 bug③：让 `serve()` 交出信号所有权

`serve()` 新增 `install_signal_handlers=True` 形参，信号注册整块包进 `if`；
`__main__.py` 调用时传 `install_signal_handlers=False`，
成为 serve 路径上 **SIGTERM/SIGINT 的唯一拥有者**。

### 2.4 配置向后兼容（本次敢直接补字段的依据）

`yaml.Decoder` 用 `KnownFields(true)` —— 只拒绝**未知**字段；
**缺字段沿用 `Default()` 的值**。所以新增配置项**不破坏线上老配置**。
用 `--check-config` 对三份 `deploy/*.yaml` 实测：
`is valid`。

三个示例配置（`deploy/tai-talea.example.yaml`、`deploy/alayanew.example.yaml`、
`deploy/kubernetes/tai-talea.yaml`）都补上了新的两个旋钮。

### 2.5 新增测试（6 条）

| 侧 | 文件 | 测什么 |
| --- | --- | --- |
| Go | `internal/controller/unhealthy_probe_test.go`（新） | 一次失败 → 仍在 SERVING 但 worker readiness 变 `unavailable`、`report.Unhealthy` 含该实例；探测恢复后计数清零。另一条：失败持续到阈值才 FAILED |
| Go | `router_role_test.go`（追加） | `h.router.removeDelay = 5s` > harness 的 `CallTimeout 3s`；断言 `StartService` 不报错、`elapsed >= removeDelay`、最终 worker 是 prefill —— **实测 6.01s**，即它确实等下来了 |
| Py | `bootstrap/tests/test_bootstrap.py`（追加 3 条） | ①`install_signal_handlers=False` 时不动外来 handler；②默认会装也会还原；③`serve` 路径确实把 `install_signal_handlers=False` 传下去 |

测试侧同时给 harness 加了能力：
`bootstrapFake.setHealthStatus()`（能模拟 `degraded`）、
`routerFake.removeDelay` + `applyPendingRemovalsLocked()`（能复现「DELETE 已接受但条目还在」）。

> ⚠️ 踩坑记录：`ReconcileOnce` **不向上抛单实例错误**（`ERROR reconcile instance failed`
> 只记日志，返回 `nil`）。所以断言必须看 `instance.ServiceState` / `ReconcileReport`，
> 不能看 `err`。首版测试就因为断言 `err != nil` 而失败。

---

## 3. 部署三步曲

| 步 | 做法 | 为什么必须这样 |
| --- | --- | --- |
| 预检 | 用**新镜像自己的** `--check-config` 跑一个 Pod，挂宿主真实配置（只读、**不挂 state 卷**） | 镜像是最小 rootfs（静态 Go 二进制，**无 shell**），`kubectl exec` 进不去也跑不了命令；唯一能执行它的办法是让 Pod 的 `command` 就是它本身 |
| 滚动 | `ns-deploy-cp.py <manifest> --digest sha256:<推送的 manifest>`：先删旧 Pod 并**轮询等它真的消失**，再 apply，再等 Ready | 控制面是 **Pod 不是 Deployment**；`--wait=false` 后紧跟 apply 会和 terminating 对象打架。而「容器在跑」不证明「跑的是新构建」——`imagePullPolicy: Always` 只保证拿到 tag 背后最新 digest，**tag 不是证据** |
| 验收 | `ns-accept-d1.py --ctrl-pod tai-talea` | 同 `d1` 文档 §4 |

### 3.1 构建与推送

```
[00:51:11] inputs: binary 12.68 MiB, config 16347 bytes
[00:51:11] layer 5.33 MiB gz (1s)  digest=sha256:4aad8b34d04a…
[00:51:16] IMAGE PUSHED: …/library/tai-talea:v0.6
           digest=sha256:c5cda830f7342d1eb7389dc8510e078e896d9bbe93e6d5e7f2c310dfc651a56d
```

二进制：`tai-talea-linux`，**13,295,776 字节**，`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`，
`-ldflags "-s -w -X …Version=v0.6 -X …Commit=a5148ec -X …BuildTime=2026-10-14T16:50:14Z"`。

构建走 `talea-control-image.py` —— **Python 标准库直拼 OCI 层**（tar+gzip，固定 `mtime`
保证可复现），registry v2 API（Bearer token、分块 PATCH、`Docker-Content-Digest`）。
**控制面镜像构建不需要 docker。**

### 3.2 预检

```
### phase=Succeeded
### exit_code=0
### imageID=…/tai-talea@sha256:c5cda830f734…c651a56d
### logs
configuration /etc/tai-talea/tai-talea.yaml is valid (1 partners, router http://10.214.81.70:8000)
```

**新二进制能解析线上真实配置** —— 这就是向后兼容的实证。
（预检 Pod 挂的是 `hostPath /data/tai-talea/config`，即线上含真 token 的那份，
不是镜像里烤入的 template。跑完已删。）

### 3.3 滚动

```
### before   …/tai-talea:v0.5   phase=Running restarts=0
### deleting tai-talea   vanished: True
### applying  pod/tai-talea created
### after: phase=Running ready=true
### imageID …/tai-talea@sha256:c5cda830f734…c651a56d
### MATCH: the running container is the build that was pushed
```

`deploy/kubernetes/tai-talea-control-plane.yaml` 的 `image:` 由 `v0.5` 改为 `v0.6`。

---

## 4. 重启接管干净 + v0.6 自证

新 Pod：`creationTimestamp=2026-10-14T16:52:04Z`、`phase=Running`、`restarts=0`、`ready=true`。

启动日志（前 5 行，逐字）：

```json
{"time":"2026-10-14T16:52:36Z","level":"INFO","msg":"planner configuration","mode":"fixed","gear":"1:1","capacity_actions":false,"provision_timeout":"45m0s"}
{"time":"2026-10-14T16:52:40Z","level":"INFO","msg":"control plane recovery finished","checked":10,"lost":0,"recovered":0,"reregistered":0,"unhealthy":0,"resumed_operations":0,"resumed_events":0}
{"time":"2026-10-14T16:52:40Z","level":"INFO","msg":"startup recovery complete","checked":10,"lost":0,"recovered":0,"reregistered":0,"resumed_operations":0,"resumed_events":0}
{"time":"2026-10-14T16:52:40Z","level":"INFO","msg":"partner pull enabled","partner_id":"cmcc-k8s","interval":"30s"}
{"time":"2026-10-14T16:52:40Z","level":"INFO","msg":"capacity control plane listening","address":"0.0.0.0:8787","router":"http://10.214.81.70:8000","gear":"1:1","partners":"cmcc-k8s"}
```

两处判据：

1. **`"unhealthy":0` 是本次新增字段** —— 老二进制不可能打印它。
   这一行的存在本身就是「v0.6 在跑」的证明，比任何版本号都硬。
2. `checked=10 lost=0 recovered=0 reregistered=0` —— 接管没有制造任何状态迁移。

之后每 30s 一条稳定的 pull：

```
partner snapshot committed  partner_id=cmcc-k8s  version=fae802d3533fd6b2  instances=2  added=0 updated=0 revoked=0
```

`version` 连续 5 个周期**逐字不变**、`added/updated/revoked` 全 0
⇒ 实例行没有抖动（这正好回应 `d1-revalidation` §5.5 那条「每轮 poll 都在重写实例行」的顾虑）。

---

## 5. 对服务零影响

观察者 `pw3`（15s 采样、30 分钟预算）横跨整个滚动窗口。关键段（逐字摘录，`|` 分段）：

```
16:50:37Z | 172229=IDLE/SERVING/prefill(a=0) 1=IDLE/SERVING/decode(a=0) | ready=200 {"healthy_workers":2,…} | util=0,…
16:51:01Z | 172229=IDLE/SERVING/prefill(a=0) 1=IDLE/SERVING/decode(a=0) | ready=200 | util=0,…
16:52:13Z | 172229=IDLE/SERVING/prefill(a=0) 1=IDLE/SERVING/decode(a=0) | ready=200 | util=0,…
16:52:37Z | 172229=IDLE/SERVING/prefill(a=0) 1=IDLE/SERVING/decode(a=0) | ready=200 | util=0,…
16:53:21Z | ?                                                         | ready=200 {"healthy_workers":2,…} | util=0,…
16:54:05Z | ?                                                         | ready=200 | util=0,…
16:54:49Z | ? | ready=200 | util=0,32,23,19,23,46,28,42
```

（每行下面还有一行进程清单：`n=14 R=<0-7> T=0 Z=0 | entrypoint,60,launch_server,main;main(48),scheduler_TP0..TP7,detokenizer`
—— 自始至终 14 个进程、**没有一次崩溃重启**。`grep -c ALARM` = **0**。）

- **Router 侧整段 `ready=200`、`healthy_workers=2`** —— 推理路径没有一刻中断。这是本节的结论性证据：
  控制面在换、实例状态偶有问不到，**但 Router 视角的可用性是一条直线**。
- 状态列从 16:53:21 起变 `?`，是观察者**自己的**局限：它启动时解析一次控制面集群 IP，
  而控制面 Pod **没有 hostNetwork ⇒ 每滚动一次地址就换**
  （`d1-revalidation` §6.3 已记录此坑）。**「问不到」不等于「没有实例」**——
  服务真相以 Router 侧为准。注：`?` 出现的具体时刻受观察者自己的轮询相位影响，
  能确定的是它出现在滚动之后，本记录不据此推断旧 Pod 的消失时刻。

---

## 6. D1 验收（3/3）

```
### control plane: the pair
   talea-cmcc-k8s-1010-172229         IDLE  svc=SERVING  role=prefill  a=0 worker=d4045cba
   talea-cmcc-k8s-1010-172229-1       IDLE  svc=SERVING  role=decode   a=0 worker=6706184e
### router pools
   stats={'prefill_count': 1, 'decode_count': 1, …}
   /readiness -> 200 {"healthy_workers":2,"available_workers":2,"total_workers":2}
### inference through the router
--- arithmetic -> 200 in 0.48s (16 tokens, "17 * 23 = 391 …")
--- chinese    -> 200 in 0.93s (64 tokens)
--- longform   -> 200 in 1.87s (160 tokens)
### verdict: 3/3 completions returned real, metered output
```

**`worker=d4045cba` 与部署前逐字相同** ⇒ 没有重注册、没有 drain、没有换角色。
`start_attempts` 仍是 0。

> 附带旁证：部署前后同一份代码连续稳定 **~26 分钟**、`ALARM=0`、`n=14 T=0 Z=0`。
> 这反过来**坐实**了上一轮「起来 46 秒就被 SIGTERM 拆掉」是**外部干扰**（节点上的
> 第三方作业）而不是我们的代码 —— 节点安静下来后，同一份代码一次异常都没有。

---

## 7. 仍未验证 / 遗留

| # | 项 | 状态 |
| --- | --- | --- |
| 1 | §5.2 那条路径（Router 未重启、成员表与库不一致 → `FindWorkerByURL` 角色不符 → `SetReadiness(false)` → `RemoveStaleMembership` → 重注册） | v0.6 的 `removal_timeout` 修复**准备好了，但还没被真实触发过**。本次 Router 没重启，`settleService` 在第一个分支就返回 `ok` |
| 2 | bug① 的 `degraded` 容忍路径 | 单测覆盖（Go 2 条）；**线上未自然触发**（节点安静，没出现探测抖动）。真出现时日志应是 `WARN … streak=1/3` 且**实例保持 SERVING**，而不是 `ERROR … running model is not healthy` |
| 3 | 线上宿主配置 `/data/tai-talea/config/tai-talea.yaml` | **手写文件，本次没动**。缺字段即走默认值（`health_failure_threshold: 3` / `removal_timeout: 8m`），所以功能上不需要改；若要显式写出需单独操作宿主 |
| 4 | worker Pod 无 `livenessProbe` | 僵死进程树 k8s 永不重启（`nightshift-outage` 文档已记）。本次未动 |
| 5 | `mode: auto` 弹性扩容 / planner 下限保护 | 代码在，**两平台都关着**（`capacity_actions=false`，日志可见） |
| 6 | `mem-fraction-static=0.85` | 偏紧，CUDA graph 捕获期 OOM 的已知诱因 |

---

## 8. 复用清单（`.workbuddy/tmp/alayanew/`）

| 文件 | 用途 |
| --- | --- |
| `tai-talea-linux` | 交叉编译产物（v0.6，commit `a5148ec`） |
| `talea-control-image.py` | 无 docker 直推 OCI 镜像；`control-image/pushed.json` 记 digest |
| `preflight-cp.yaml` + `.preflight-cp.sh` | 用新镜像自己跑 `--check-config`，挂宿主真实配置只读 |
| `ns-deploy-cp.py` | 删旧 Pod → 等消失 → apply → 等 Ready → **imageID 逐字比对 digest** |
| `ns-accept-d1.py` | D1 验收（控制面/ Router / 经 Router 推理三段） |
| `ns-hold.py` + `.readpw3.sh` / `.cp-proof.sh` | 常驻观察者与取证脚本（经 `nrsh.py` 在 n1 上跑） |

**纪律**：探针一律 `nsrun.py <driver> --ctrl-pod tai-talea`（动态解析 Pod IP）；
长驻观察者跨不过滚动，滚动后必须重启它。
