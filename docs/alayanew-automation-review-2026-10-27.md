# 九章 CCI 自动部署链路 review（2026-10-27）

## 0. 结论

**现在还不能自动推理部署。**

把「平台里还没有容器」到「客户端能出 token」拆成 11 段，逐段对代码核对的结果是：

- **后 5 段（入库 → 分角色 → 起 sglang → 注册 Router → 就绪）已经是自动的**，而且 10-26 的 1P1D
  就是这套逻辑自己收敛出来的，不是手工摆出来的。
- **前 3 段（决策扩容 → 开容器 → 拿到实例 id）一段也走不通**：两个开关关着，加上一个已经停用的平台接口。
- **中间 3 段（铺装运行时 → 节点自检）在仓库里没有可用制品**，只有「设计好的闸门」。

| 目标 | 现在能否自动 | 卡在哪 |
| --- | --- | --- |
| 平台空池 → 自动开一个容器 | ✗ | `capacity_actions=false` + `onboarding` 未配置 + `WaitCreation` 调已停用接口 |
| 已有容器 → 自动把运行时铺上去 | ✗ | wheelhouse 未构建（`wheels/` 只有 README） |
| 运行时铺好 → 自动出流量 | ✓ | 已验证（10-26 验收） |
| 进程挂了自愈 | ✗（CCI 上） | 控制面 / Router 无守护，容器重启清 overlay |

一句话：**「把节点交进来之后」全自动，「把节点搞到手、把运行时装进去」全手工。**

---

## 1. 现场基线（review 时刻）

| 项 | 状态 |
| --- | --- |
| 实例 | `glm53-pd2`、`glm53-pd3`，均 `RUNNING`（九章控制台实测） |
| Router | `prefill_count=1`、`decode_count=1`，两个 worker `healthy=true` |
| 真实请求 | `7*8` → `56`，`finish=stop` |
| 控制面 | `tai-talea 0.9.0-cci`，8787 监听在 pd2 |
| 运行时 | `sglang 0.5.22` + `torch 2.14.1+cu130`（`/opt/tai-talea/venv`） |
| profile | `ubuntu-22.04 / cuda 12.9 / python 3.12 / sglang 0.5.22` |

本次 review 全程只读，未改动任何实例、配置或进程。

---

## 2. 逐段核对

| # | 阶段 | 代码位置 | 判定 | 依据 |
| --- | --- | --- | --- | --- |
| 1 | 判断要扩容 | `internal/planner` | ✗ 关着 | 线上 `planner.mode: fixed`；`capacity_actions` 未设，且 `planner.go:181` 明确 fixed 模式禁止 capacity 动作 |
| 2 | 向平台开容器 | `adapter.go:298 CreateInstance` → `client.go:217` | ✓ | 请求体 `{productCode, cciParams}`，10-26 探测单实测 200 |
| 3 | 等受理、拿 instanceId | `client.go:233 subscriptionResultOnce` → `adapter.go:346 WaitCreation` | ✗ **必断** | 调的是已停用接口（见 G1） |
| 4 | 等 RUNNING 并取 SSH | `adapter.go:224 AwaitSSHAccess` | ✓（依赖 #3） | `QUEUED` 时**停表不计数**，45m 上限后返回 `ErrAwaitingCapacity` → job 被 `hold` 而非释放。语义正确 |
| 5 | SSH 铺装 | `provision/ssh_install.py` + `provision/install_node.py` | ✗ **必断** | 制品链为空（见 G2） |
| 6 | 节点自检 | `bootstrap/tai_talea_bootstrap/profile.py` | ✗ 口径不符 | 仓库默认 0.5.20 / 24.04 / cu13，现场 0.5.22 / 22.04 / cu12.9（见 G3） |
| 7 | 入库 | `internal/onboarding/manager.go:322 m.enroll` | ✓ | **铺装成功直接入库**，用安装时实测的内网地址，不依赖 pull |
| 8 | 分 P/D 角色 | `planner.go:774-798` | ✓ | fixed 模式下同样工作：`idle instances fill whatever the ratio still needs` |
| 9 | 起 sglang | `lifecycle.go:236` → `roles.py:145` | ✓ | `--disaggregation-mode` 由 bootstrap 按角色注入，配置里写会 422 |
| 10 | 注册 Router | `lifecycle.go:397 register` + `reconcileServiceMembership` | ✓ | 实测 Router 重启后 worker 25s 内自动回位 |
| 11 | 公网入口 | — | ✗ 手工 | 全仓只有 `open-port/list`（读），没有任何地方开端口；且无鉴权 |

> 关于 #7：这一点推翻了我此前的一个判断。「不开 9001 ⇒ pull 跳过 ⇒ 节点不可用」是**错的**。
> `m.enroll` 在铺装成功时直接写入 store，pull 只负责容量快照；快照里缺实例时
> `events.go:544` 只发 `AlertPartnerSyncFailed` 告警，**不会**释放节点（`handleSnapshotConfirmed` 只审计 + 告警）。
> 所以没开端口后果是「每 60s 一条告警噪音」，不是「节点白买」。

---

## 3. 三个硬断点

### G1 `WaitCreation` 调用了已停用的接口，且响应结构也变了

```go
// client.go:233
err := c.call(ctx, "GET", "/api/osm/v1/subscription/CCI/result/"+key, "", nil, &out)
```

平台已停用该路径，返回：

```json
{"status":212147,"message":"该接口已停用，请改用 /v1/subscription/sku/cci/result/{resultKey}"}
```

可用的新路径是 `GET /api/osm/v1/subscription/sku/cci/result/{key}`，但**返回结构不是同一个**：

```json
{"data":{"status":"SUCCESS","resources":[{"status":"RUNNING",
  "errorCode":0,"errorMessage":"","resourceInstanceId":"...","subscriptionNo":"..."}]}}
```

旧结构 `{result, message, instanceId}` 直接套上新路径会解析出空 `instanceId`，
`adapter.go:357` 于是报 `creation succeeded without instanceId` —— 一个看起来像平台问题、实际是解析问题的错误。

**改法**（`internal/partners/alayanew/client.go` + `adapter.go`，约 60 行 + 测试）：

1. 路径换成 `/api/osm/v1/subscription/sku/cci/result/`，key **原样**进路径（含冒号，不能 URL 编码，否则签名不匹配）。
2. 结构改成 `data.status` + `data.resources[]`，取 `resources[0].resourceInstanceId`。
3. `resources[].errorCode != 0` 时**把平台原话透出来**（例如 `40010 剩余配额为 0，已达上限 3`），而不是笼统的 FAILED。
4. **不要把「实例还在 QUEUED」当失败**：`WaitCreation` 的职责是「平台已受理并给了 id」，
   排队交给 #4 的 `AwaitSSHAccess`，它已经会在排队期间停表。

### G2 自动铺装的制品链在仓库里是空的

`provision/ssh_install.py` 在控制节点侧先过一道闸门：

- 需要 `onboarding.manifest` 指向的 manifest，内含 `wheelhouse_source`、`wheel_manifest_sha256`、`requirements_sha256`；
- 需要 `wheelhouse_source/manifest.json`，逐个比对 SHA256；
- **拒绝任何非 `.whl` 的文件**；
- `profile.virtualenv` 必须是 `/opt/tai-talea/venv`、`profile.wheelhouse` 必须是 `/opt/tai-talea/wheelhouse`。

节点侧 `provision/install_node.py` 只做一件事：

```python
run([venv/bin/python, '-m', 'pip', 'install', '--no-index',
     '--find-links=' + profile['wheelhouse'], '-r', profile['wheelhouse'] + '/requirements.txt'])
```

而仓库现状：

| 需要的东西 | 现状 |
| --- | --- |
| `provision/wheelhouse/wheels/*.whl` | **目录里只有 README.md**，一个轮子都没有 |
| `provision/wheelhouse/requirements.txt` | **不存在**（由 `build.py` 生成） |
| `manifest.json` + `wheel_manifest_sha256` | **不存在** |
| `onboarding-manifest.json` | **不存在** |

⇒ 今天不管点哪个按钮，都会在「上传前」就被自己的闸门挡住。**设计是对的，制品没有。**

### G3 profile 口径与 CCI 现场不一致

三处 profile，两套值：

| 来源 | os | cuda | sglang |
| --- | --- | --- | --- |
| `deploy/container/profile.json`（打进镜像） | ubuntu-24.04 | 13.x | **0.5.20** |
| `provision/wheelhouse/spec.json`（构建规格） | ubuntu-24.04 | 13.x | **0.5.20** |
| 线上控制面 `image_profile`（CCI 现场） | **ubuntu-22.04** | **12.9** | **0.5.22** |

后果分两种，第二种更坏：

- 若按仓库默认建 wheelhouse → 装出来是 0.5.20，与线上 `image_profile` 不一致；`verify.py --profile` 会拦住，
  或者节点自检按自己的 profile 通过、后面加载模型才死。
- **已经确定的事实**：镜像自带的 0.5.3rc0 两棵源码树都没有 `glm5_next.py`，加载不了 GLM-5.3-Flash；
  现场唯一验证过的是 **0.5.22**。仓库默认的 0.5.20 **从未在 CCI 上验证过**。

⇒ 自动化之前必须先把 profile 统一到 CCI 实测值：`ubuntu-22.04 / cuda 12.9 / python 3.12 / sglang 0.5.22`。

---

## 4. 两个关着的开关（不是 bug，是配置）

| 开关 | 现状 | 影响 | 打开方式 |
| --- | --- | --- | --- |
| `planner.capacity_actions` | 未设（false） | planner 永不发 `PROVISION` / `RELEASE` | 需同时 `planner.mode: auto`，否则 `planner.go:181` 直接拒绝 |
| `onboarding` 段 | **整段缺失** | `POST /v1/provision` 与 `POST /v1/capacity/console/onboarding` 都回 503「控制节点尚未配置自动安装制品」 | 配 `python/script/manifest/state_dir` 四项 |

`GET /v1/capacity/console` 的 `onboarding_enabled` 就是 `s.opts.Onboarding != nil`，现在应为 `false`。

---

## 5. 三个不是断点、但会咬人的地方

### N1 端口没人负责

`toCapacityInstance` 要求 `bootstrap_port`(9001) 已发布，否则该实例从 pull 快照里被跳过。
全仓只有 `open-port/list`（读），**没有开端口的调用**；创建请求体里也没有端口字段。

实际影响有限，因为控制面与 worker 同在 CCI 内网（172.19.x.x），端点是安装时实测的内网地址、
由 `EnrollPrepared` 写入，不需要公网映射。但：

- 每个自动创建的节点都会让 pull 每 60s 报一条 `tracked instances are missing from the confirmed partner snapshot`；
- 客户端要访问 Router，仍然必须手工 `open-port`。

### N2 指标开关只在白名单里，不在默认 argv 里

`roles.py` 的 `build_sglang_command` 基础 argv 只有 `--model-path/--host/--port/--disaggregation-mode`；
`--enable-metrics`、`--enable-cache-report` 只出现在 `ALLOWED_EXTRA_FLAGS`（允许，但不自动加）。

漏了不会报错，后果是静的：

- 缺 `--enable-metrics` → `/metrics` 404 → 负载输入**静默关闭**；
- 缺 `--enable-cache-report` → Router 的就绪视图残缺。

线上配置里这两个是显式写着的（第 89-90 行），但这是**记忆里的知识**，不是代码保证。
新增节点如果只配 role 级参数、漏了全局那两个，会得到一个「能服务但看不见」的 worker。

附带一个同类陷阱：`validate_extra_args` 用 `shlex.split`，所以
`--model-loader-extra-config={"concurrency":16}` 里的双引号会被吃掉，
必须写成 `--model-loader-extra-config='{"concurrency":16}'`。

### N3 控制面与 Router 没有守护

CCI 容器停止/重启会清空 overlay：`/etc/tai-talea`、`/opt/tai-talea`、`/root/tai-talea` 全部消失，
只有 NAS（`/root/gongji`）存活。恢复靠两个手写脚本：

1. `cci-unpack-runtime.sh` —— 从 NAS 解包 venv + bits + JIT 缓存，实测 **1 分 46 秒**；
2. `cci-start-plane.sh` —— 起 Router（带 6 个非默认 flag）和控制面。

这两个脚本在 `.workbuddy/tmp/alayanew/` 下，**不在仓库里**，也没有被任何进程或探针拉起。
更麻烦的是它们替代不了自动化：`pkill -x sgl-model-gateway` 永远不生效（`comm` 被截到 15 字符），
旧 Router 会继续占 8000，而 `curl :8000/health` 的 200 是**旧进程**在应答 —— 假成功。
脚本里靠 `ss -lntp` 比对 PID 才判得准，这套判据目前只存在于这个 shell 脚本里。

---

## 6. 现在从零到 1P1D，实际要几步

| 步 | 做什么 | 谁做 |
| --- | --- | --- |
| 1 | 平台上开 8 卡实例（EXCLUSIVE，弹性拿不到卡） | **人**：控制台或 `cci_client.py` |
| 2 | 铺运行时：`cci-unpack-runtime.sh` | 脚本（手工触发） |
| 3 | 起 Router + 控制面：`cci-start-plane.sh` | 脚本（手工触发） |
| 4 | 把节点交给控制面 | **不能自动**：要么 `POST /onboarding`（要制品），要么改 `instances:` 静态清单 |
| 5 | 入库 → 分角色 → 起 sglang → 注册 Router | 自动 |

⇒ 4 步里 2 步是脚本、2 步是人，而第 4 步正好是唯一能一键化却因为制品缺失走不通的那一步。

---

## 7. 最小改造清单（按性价比排序）

| 编号 | 改动 | 面 | 风险 | 说明 |
| --- | --- | --- | --- | --- |
| **M1** | 修 `WaitCreation`：换路径 + 换结构 + 透出平台错误码 | Go，2 文件 ~60 行 + 测试 | 低 | 唯一必须改 Go 才能继续的地方。改完 `POST /v1/provision` 才可能过第 3 段 |
| **M2** | 给 `ssh_install.py` 加 **runtime-tarball 分支** | Python，1 文件 ~150 行 | 中 | 只上传脚本（<100KB），运行时从**已挂载的 NAS** 解包。绕开 wheelhouse，直接复用 10-26 验证过的 1 分 46 秒路线。**必须覆盖 bits tar 里带的旧 `bootstrap-token`**，否则与 `controller.bootstrap_token` 不一致 → 401 |
| **M3** | 三处 profile 统一到 CCI 实测值 | 配置，3 文件 | 低 | `0.5.22 / ubuntu-22.04 / cuda 12.9 / python 3.12` |
| **M4** | 打开两个开关：`onboarding:` 段；若要按需扩缩再加 `mode: auto` + `capacity_actions: true` | 配置，1 文件 | 中 | 只开 onboarding 也能做到「人开容器 → 全自动上线」 |
| **M5** | 控制面与 Router 的自启/自愈 | shell / 平台能力 | 中 | **待查**：CCI 创建实例是否支持覆盖启动命令。若支持，把「解包 + 起进程」写进启动命令，N3 一次性解决 |
| **M6** | 创建后调 `open-port` 发布 9001（若还要 pull 认领）或只是消警 | Go，1 文件 ~30 行 | 低 | 不影响内网可达性，优先级最低 |

M2 之所以优于「按 `spec.json` 建 wheelhouse」：wheelhouse 是 sglang 0.5.22 + torch cu130 的全量轮子，
需要 CUDA 12 环境做 manylinux 交叉构建，产出数 GB，每次铺装还要上传数 GB；
NAS 上那套 tar 已经验证过、只需要上传 3 个小文件。

---

## 8. 建议的验收方式（不改动现役 1P1D）

1. 先做 M1 + M3 + M2，控制面用**新二进制**在 pd2 上原地重启（不改拓扑）。
2. **测试前先把 `planner.max_serving: 2`**：配额允许再开 1 个实例，但若 max_serving 仍为 0，
   planner 会把第 3 台也分上角色 → 现役 1P1D 变 2P1D，等于拿线上服务做实验。
3. 开第 3 台（EXCLUSIVE 8 卡），只调 `POST /v1/capacity/console/onboarding`：
   传 `{ID: <实例名>, LeaseID: "cci-<实例名>", Host, Port, User, Password, PartnerID: "alayanew"}`。
   **判据**：job 走到 `环境就绪，提交纳管`；`/v1/capacity/instances` 出现该行且
   `instance_state=preparing`，端点是实测内网地址（不是平台公网 ingress）。
4. 恢复 `max_serving`，观察它是否被自动分到 prefill（2P1D），穿 Router 发一个请求；
   或者直接 release 掉，只留验收记录。
5. 最后一步才做「从零全自动」：清空实例 → `POST /v1/provision` → 等 1P1D 自己起来。
   这一步必然要停现役服务，放在独立窗口做。

---

## 9. 本次 review 更正的两处旧结论

| 旧结论 | 更正 |
| --- | --- |
| 「订阅 SUCCESS ≠ 实例 RUNNING ⇒ `AwaitSSHAccess` 空等 30m」 | `AwaitSSHAccess` 在 `QUEUED` 时**停表**，只有 outer ctx（默认 45m）能终止，且返回 `ErrAwaitingCapacity` → job 被 `hold` 不释放。真正的问题只是 M1 那个解析 |
| 「不开 9001 ⇒ pull 跳过 ⇒ 不会被分配角色」 | 对**探测单**成立（没走纳管，只靠 pull 发现）；对**正规纳管**不成立：`m.enroll` 直接入库，pull 缺失只发告警 |

（`docs/alayanew-capacity-probe-2026-10-26.md` 已同步更正。）

---

## 10. 一句话建议

**别去建 wheelhouse。** 把 M1（换接口）、M3（统一 profile）、M2（NAS tar 安装器）做完，
`POST /v1/provision` 这条路就能端到端跑通；M4 只决定「要不要让它自己去要容器」。
M5/M6 是稳定性与入口，不影响「能不能自动」。

---

## 11. 本轮已落地（2026-10-27 14:50–15:10）

### M1 `WaitCreation` 换接口 —— 已改、已测、已上线

`internal/partners/alayanew/client.go` 与 `adapter.go`：

- 路径换到 `/api/osm/v1/subscription/sku/cci/result/{key}`；
- 解析改成 `data.status` + `data.resources[]`，取 `resourceInstanceId`；
- 平台拒绝时透出原话（`status 40010: 您的剩余配额为：0，已达上限：3…`）；
- **`SUCCESS` 不再等 RUNNING**：实测 CPU 订阅在 create 后 1 秒就报 `SUCCESS`，
  而容器还在建。现在 id 一到手就返回，排队交给 `AwaitSSHAccess`（它会停表）。

新增三个测试：平台拒付原话透出、`SUCCESS` 立即返回 id、以及原有的 FAILED 用例。
全量 `go test ./...` 与 `go vet ./...` 通过。

**已部署**：`0.9.1-cci`（`bin/tai-talea-linux-1027`）已在 pd2 原地替换并重启。
重启日志 `control plane recovery finished checked=2 lost=0 recovered=0`，
两个实例仍 `IDLE/SERVING`、角色未变，Router 两个 worker 未受影响，`8*8` → `64`。
旧二进制备份在 `/root/tai-talea.bak-1027`。

### M3 profile 统一 —— 已改

- `deploy/alayanew.example.yaml` 的 `image_profile` 改为实测值
  （`ubuntu-22.04 / cuda 12.9 / python 3.12 / sglang 0.5.22`）。
  这个文件原来的注释写着「Measured on the H100 CCI container」却填着 24.04/cu13/0.5.20，
  是从通用示例复制过来的。
- 新增 `provision/cci/profile.json`（现场 `/etc/tai-talea/profile.json` 原文）。
- `provision/wheelhouse/spec.json` 的 pin **没动**：它是私有镜像 + k8s 通道的规格，
  在注释里明确了两条通道各自的值，避免下次有人再"统一"错方向。

### M2 NAS 运行时通道 —— 已改、已测、制品已就位

`provision/ssh_install.py` 增加第二条通道，manifest 里出现 `runtime` 就走它：

- 只校验并解包共享卷上的 tar，**不要求任何 wheelhouse 制品、不上传任何大文件**；
- 解包前按卷上的 `SHA256SUMS` 核验（缺失则拒绝，除非显式 `checksums_required: false`）；
- 解包后重写 `/etc/tai-talea/profile.json` 与 `bootstrap-token`
  —— tar 里带的是烤制当时的旧 token，不换就是每个 start 请求 401；
- 按 `purge` 删除 `/etc/tai-talea/tai-talea*.yaml`：**这个文件含平台 AK/SK**，
  worker 从不读它，留着等于把控制面凭据复制到每台推理机；
- 用系统 `python3` + `bootstrap-src` 作 cwd 启动 bootstrap（与现役进程逐字一致）。

配套：`provision/cci/manifest.example.json`、`provision/cci/README.md`，
以及 7 个新测试（缺 tarball、相对路径、路径穿越、相对解包目录、purge 校验、
runtime manifest 通过闸门、wheelhouse 通道仍守原闸门）。

**制品已部署到控制节点**：`/opt/tai-talea/provision/` 三个脚本、
`/etc/tai-talea/onboarding-cci.json`、以及带 paramiko 5.1.0 的
`/opt/tai-talea/provision-venv`。

**干跑验证**（目标故意选 pd2 自己）：`{"stage":"校验控制节点安装制品"}` →
`{"stage":"连接 SSH，核对容器环境"}` → `{"error":"不能把 Talea 控制节点上线为推理节点"}`。
四件事同时被证明：制品闸门接受 runtime manifest（不再要 wheelhouse）、
SSH 用平台密码认证成功、PROBE 正常、守卫在**任何写入之前**生效。

### 还没做

| 项 | 状态 | 说明 |
| --- | --- | --- |
| M4 打开开关 | 未做 | `onboarding:` 一段即可；`capacity_actions` 还要 `mode: auto` |
| 解包+启动的真机验证 | 未做 | 需要一台空闲实例；会改变线上拓扑，须先 `max_serving: 2` |
| M5 控制面/Router 自启 | 未做 | 待查 CCI 创建实例能否覆盖启动命令 |
| M6 端口 | 未做 | 内网可达性不需要它，只影响 pull 快照与公网入口 |

工作树尚未提交（二进制里的 `commit` 是 HEAD 的 `0b7b5e4`，但代码含未提交改动），
版本号用 `0.9.1-cci` 区分。
