# nightshift 全自动推理部署：缺口清单（2026-10-09）

> 回答一个问题：**「让 talea 在 nightshift k8s 上自动完成 GLM-5.3-Flash 1P1D 部署，还差什么。」**
> 依据：`internal/partners/kubernetes/`、`bootstrap/tai_talea_bootstrap/`、`deploy/kubernetes/` 现有代码，
> 加上 2026-10-09 在 h100-01/h100-04 上纯手工验证通过的全套真实 manifest
> （`.workbuddy/tmp/alayanew/glm53-pd-{prefill,decode,router,bench}.yaml`）。

## 1. 已经成立的部分（不用再写）

| 能力 | 落点 | 状态 |
| --- | --- | --- |
| SelfProvisioningAdapter：建 Pod=铺装，租约 `k8s-<podUID>`，归还=删 Pod | `internal/partners/kubernetes/adapter.go` | ✅ 代码在 |
| Pod 构建：镜像、GPU limits、nodeSelector、hostPath 卷、512Gi shm、entrypoint+token 注入、9001 就绪探针 | `manifest.go` | ✅ 代码在 |
| 控制面开通入口分流（ProvisionNode → EnrollPrepared，无 onboarding 段） | `internal/api/provision.go` | ✅ 代码在 |
| 生命周期：prepare→start（`/bootstrap/start` 下发角色）→注册 Router→readiness 摘流；start_timeout 15m | `internal/controller/` | ✅ 代码在 |
| bootstrap 角色命令组装：`--disaggregation-mode prefill/decode` 自动加 + 白名单参数 | `bootstrap/tai_talea_bootstrap/roles.py` | ✅ 代码在 |
| 基线身份三处一致：`ubuntu-24.04 / cuda 13.x(通配) / python 3.12 / sglang 0.5.20` | `deploy/kubernetes/tai-talea.yaml` image_profile | ✅ 与 `lmsysorg/sglang:v0.5.20-cu130-runtime` 吻合 |
| 镜像已在节点上：sglang 0.5.20 官方镜像已复制进两台机器的 `k8s.io`（ctr export/import 链路已验证，`imagePullPolicy: Never`） | 节点 containerd | ✅ 今天验证 |
| mooncake-transfer-engine 在镜像内（PD 硬依赖） | 同上 | ✅ 今天隐式验证 |
| 配置模板已带 GLM 配方（tp8/ep8/dsa/deep_gemm/mem0.60/chat-template…） | `tai-talea.yaml` extra_sglang_args | ✅ 大部分在 |
| Router 同机部署模板（hostNetwork 固定地址、聚合模式、动态注册） | `talea-router.yaml` | ✅ 模板在（有两处要修，见 §3） |

## 2. 缺口总览

```
┌─ 平台/制品层 ────────── M1 控制面进集群  M2 worker 侧 bootstrap 供给  M3 Router 二进制
├─ adapter 代码层 ──────── A1 特权/宿主开关  A2 按节点/按角色参数与 env
├─ bootstrap 代码层 ────── B1 参数白名单扩充
├─ Router 侧 ──────────── C1 动态注册 readiness 实测  C2 权重挂载
└─ 验收层 ──────────────── D1 控制面驱动 1P1D 端到端  D2 摘流/恢复场景
```

## 3. 逐项缺口

### ~~M1 控制面落进集群（阻塞一切）~~ ✅ 已完成 2026-10-09 晚

**控制面已在集群里 Running**（Pod `tai-talea` @ h100-04，1/1，0 重启）。
详见 `docs/control-plane-nightshift-2026-10-09.md`。

实际走的路线比这里最初设想的更干净：**不再需要 hostPath 投放二进制**，
因为同一天下午打通了 ecis registry，控制面直接做成了一个小镜像（5.29 MiB gz）：

| 项 | 值 |
| --- | --- |
| 镜像 | `shenhaoyuan-73767fb9.ecis.shanxi-1.cmecloud.cn/library/tai-talea:v0.1` |
| manifest | `sha256:4ac1f12946d49b713e502c48eb0ffc4cd3a4218324524388fed7e717396080fd` |
| layer | `sha256:abaf657962ea…` / 5.29 MiB gz |
| 内容 | 静态 Go 二进制（13 MB，CGO_ENABLED=0）+ 默认 config + 最小 /etc，**无 shell** |
| 脚本 | `.workbuddy/tmp/alayanew/talea-control-image.py`（纯 stdlib，无 docker） |

kubeconfig 仍走挂运维凭据这条路线（`/data/tai-talea/config/kubeconfig`），
长期正解（让平台给控制面 SA 授权）不变。

**四条独立验证**：① kubelet 从 ecis 自动拉取（imageID digest 逐字一致）；
② 静态二进制在无 shell 容器里跑起来；③ 配置解析 + SQLite 落 hostPath；
④ **每 30s 一次 `partner snapshot committed`** —— 控制面真在指挥集群。
API：`GET /v1/capacity/console/instances` + Bearer admin_token → `200 {"count":0}`。

**八处配置修正**见交付文档 §3，其中第 5 条（`profile_path` 必须指
`/opt/tai-talea/profile.json`）不改会导致 exit 29 实例被释放。

工作量：实际约 1 人日（含镜像管线复用）。


### M2 worker 侧 bootstrap 供给（两条路线二选一）

worker Pod 必须自带 `/opt/tai-talea/entrypoint.sh + tai_talea_bootstrap + profile.json`。

- **路线 A（推荐先用，零镜像构建）**：bundle 放节点 hostPath（chroot 投放一次），
  adapter `Config.Entrypoint` 已支持覆盖、`Volumes` 已支持追加挂载 → 纯配置解决。
- **路线 B（干净，对齐受控镜像路线）**：在 `lmsysorg/sglang:v0.5.20-cu130-runtime`
  上叠 `deploy/container/Dockerfile` 构建 `talea-worker` 镜像。本机无 docker，
  需要群内可写的 Harbor 账号（`registry.paas/cmss` 匿名仅 pull）或平台代建。
  这正是 wheelhouse「build once / ship wheels」的载体问题，已有 README 约定。

> **补充（2026-10-09 下午，读了《GLM-5.3-Flash H100 部署与性能总结 2026-09-23》后修正）**：
> 我们**已经有一个自建镜像** `base-mirror.tencentcloudcr.com/mode-optimization/glm53-h100:v0.1`
> （11.2 GiB，2026-09-23 推上腾讯云 TCR），且构建管线是**纯 Python 标准库构建 OCI + Registry v2 API
> 推送，全程无需 docker**（构建脚本 `glm53-image.py` 在九章容器 `/opt/glm53/scripts/`）。
> 这条管线可以直接复用来产出真正的 talea-worker 镜像并推进任意 Registry v2（含 Harbor）。
>
> 但该镜像**不能直接用于 nightshift**，四个错位：
> 1. **air-gap**：TCR 是公网站点，nightshift 节点拉不到；n1 有外网、可经 Registry v2 API
>    纯 Python 拉取后再走已验证的 ctr tar 链进城，或推进 Harbor（若节点实时 pull 验证通过）。
> 2. **没有 talea bundle**：内容是 `glm53-venv + sglang-main-src + scripts`，缺
>    entrypoint.sh / tai_talea_bootstrap / profile.json —— 它是运行时镜像，不是 talea-worker。
> 3. **sglang 来源不同**：镜像里是上游 main 源码（editable），不是 nightshift 验证过的
>    内部 fork 0.5.20（5a26fc1f）——内核行为（DSA/deep_gemm/PD）不能视为已验收。
> 4. **基线身份错位**：宿主环境 alayanew Ubuntu 22.04 / Python 3.11 系，与 nightshift
>    image_profile（ubuntu-24.04 / python 3.12）不符，直接跑会触发容器自检 exit 29。
>
> 结论：v0.1 的价值在**管线**（无 docker 的构建+推送+拉取）而不在**镜像本身**。
> 正确产物应是：sglang 0.5.20 官方镜像（已在 k8s.io）+ bundle 层，用同一条 Python 管线
> 构建并推进 nightshift 可达的 registry。

> **✅ 已完成（2026-10-09 晚）：`talea-worker-v0.1` 已产出并推送。**
> 在九章 prefill 容器上，用同一条纯 Python 管线，直接从既有的 `/opt/tai-talea` 运行时树
> （venv 9.4 G + wheelhouse 3.9 G + bootstrap + entrypoint + profile）打出镜像：
>
> | 项 | 值 |
> | --- | --- |
> | 镜像 | `base-mirror.tencentcloudcr.com/mode-optimization/glm53-h100:talea-worker-v0.1` |
> | manifest | `sha256:1cd1ec54a0ba83ca80fb0c98017fe6c9c4de3d5e5a5f4c003506db5e9b09a924` |
> | layer | `sha256:27dd544a6278...` / 7.72 GiB gz（12.59 GiB raw） |
> | 构建脚本 | `.workbuddy/tmp/h100-glm53/talea-worker-image.py` |
>
> 四个错位**全部修掉**：内容就是 talea bundle（含 entrypoint/bootstrap/profile，
> 路径 `/opt/tai-talea` 与 partner 契约一致）；profile.json 是
> `ubuntu-24.04 / cuda 13.x / python 3.12 / sglang 0.5.20`，与 `image_profile` 逐字段一致；
> 构建机实测 Ubuntu 24.04.3 + Python 3.12.3 + sglang 0.5.20 + CUDA 13.0.88 + H100 sm_90。
>
> 三层验证全过：registry manifest 200 可解析 / 回拉 sha256 **MATCH** / 解包后
> venv 真实 import 全通（sglang 0.5.20、torch 2.13.0+cu130、mooncake、tilelang、
> deep_gemm、flashinfer、uvicorn、fastapi 等）+ `pip check` 干净。
> 详见 `docs/talea-worker-image-2026-10-09.md`。
>
> ⚠️ 两个使用前提，务必记住：
> 1. **venv 依赖宿主 `/usr/bin/python3`。** `venv/bin/python → python3 → /usr/bin/python3`，
>    `pyvenv.cfg` 固定 `home=/usr/bin`、`version=3.12.3`。基础镜像必须是 Ubuntu 24.04
>    （系统 python3 = 3.12.3），换 minor 版本即失效。镜像自带的是 bundle，不是 OS。
> 2. **进城通道仍未验证。** 本镜像在腾讯云 TCR，nightshift air-gap 够不着；
>    ecis Harbor 只有 pull 权限、且节点实时 pull 从未成功（历史 Pod 全是 `Never`）。
>    见下条。

注意 profile.json 必须与 image_profile 三元组一致，否则容器自检 exit 29。
工作量：A 0.5 天；B 视 Harbor 权限另计（**构建侧已完成**，剩余为进城与实时 pull 验证）。

⚠️ **「上传镜像」通道本身未验证**：节点实时 pull 从未试过——全部历史 Pod 都是
`imagePullPolicy: Never`（镜像预先放进 containerd）。走镜像路线前先花 5 分钟验证：
push 一个小镜像到 `registry.paas/cmss`（或 ecis Harbor），起 Pod 用 `Always` 看能否拉取。
通则镜像路线成立且新节点免搬运；不通则继续 ctr tar 链（每节点一次，已验证）。

### ~~A1 adapter 缺宿主/特权开关~~ ✅ 已完成 2026-10-10

`Config` 加 `HostNetwork / HostIPC / Privileged`（默认 false）+ wire 类型补字段 +
`manifest.go` 渲染，hostNetwork 时自动带 `dnsPolicy: ClusterFirstWithHostNet`。
`/dev/infiniband` 不用改代码——现有 `Volumes`（hostPath）就能表达。

**集群验收**：控制面 `provision` 建出的 worker Pod，spec 里四项全在，且 **Pod IP 等于 h100-04 宿主 IP**。
详见 `docs/worker-host-access-and-allowlist-2026-10-10.md`。

### ~~A2 按节点 / 按角色的参数与 env~~ ✅ 已完成 2026-10-10（代码 + 模板 + 测试 + **真机端到端**）

**两台机器的配置并不相同**，而 `Config.Env / extra_sglang_args / NodeSelector`
原本都是「整个 partner 一份」：

| 项 | h100-01（prefill） | h100-04（decode） |
| --- | --- | --- |
| `--disaggregation-ib-device` | `mlx5_2,3,4,5,7,8,9,10`（**必须剔除 mlx5_6**，坏卡） | `mlx5_0,1,2,3,5,6,7,8` |
| `NCCL_IB_HCA` / `NCCL_IB_TC` | 不设 | `mlx5_0,1,2,3,5,6,7,8` / `106` |
| 独有参数 | `--chunked-prefill-size 512` | `--max-running-requests 128`、`--cuda-graph-max-bs-decode 128` |

实际实现（细节见 `docs/role-and-node-argument-overrides-2026-10-10.md`）：

- **角色级**：`controller.extra_sglang_args_by_role`，在 `StartService` 构造 `StartRequest`
  时按**旗标**合并。是覆盖不是追加——SGLang 用 argparse，重复旗标合法且只取最后一个，
  追加式覆盖会生成一个语义取决于参数顺序的命令行
- **节点级**：`kubernetes.nodes[].env`，创建 Pod 时按 `固定注入 < partner env < node env`
  三层合并（越具体越优先）
- **节点轮转**：`nodes` 非空时每建一个 Pod 前进一格（游标是原子的），两台机的 8 卡各归一个
  worker；`nodes` 为空则完全退回旧的 `node_selector` 单机行为
- Pod 打 `tai-talea.node: <host>` 标签，不用翻 spec 就能看出落点
- 模板另把 `--mem-fraction-static` 从 `0.60` 改为 `0.85`（PD 布局的验证值；
  0.60 属于聚合布局，改错的表现是 warmup 阶段的 illegal memory access 而不是 OOM）

⚠️ **边界**：`--disaggregation-ib-device` 的真实作用是「按机器」而非「按角色」——
两台机有同名但物理不同的卡。放在角色级成立，仅因为当前「角色与节点一一对应」。
换机器必须同时改两处取值，文档与模板注释都已写明。

> **前置（2026-10-10 补）**：A2 没改 `roles.py`，但改了控制面代码 → 仍要重打
> `tai-talea:v0.3`（**尚未打**）。worker 镜像的 bundle 快照纪律不变：
> 每改一次白名单就必须重打镜像、换 tag（B1 踩过一次）。
>
> 新增的一条防护：`bootstrap/tests/test_bootstrap.py` 里
> `test_every_flag_in_the_deployment_template_is_allowed` 会**从模板正则提取全部旗标
> 逐个过白名单**，把「模板改了但白名单没跟上」变成测试失败，而不是 worker 起来之后的 422。

### ~~B1 bootstrap 参数白名单扩充~~ ✅ 已完成 2026-10-10

`roles.py ALLOWED_EXTRA_FLAGS` 补齐（19 → 35 项）：

```
--disaggregation-ib-device        # RDMA 设备钉扎（A2 的载体）
--cuda-graph-max-bs-decode        # 0.5.20 拆分后的新旗标
--disable-shared-experts-fusion
--enable-cache-report
--served-model-name
```

`deploy/kubernetes/tai-talea.yaml` 里的 `--disaggregation-transfer-backend=mooncake_tcp` 已删除
——内部 fork 硬编码 mooncake rdma，取值 tcp 会被 sglang 直接拒（白名单匹配的是旗标不是取值，
所以旗标本身留着无害）。

> ⚠️ **同轮发现的独立缺陷**：worker 镜像 `talea-worker:v0.1` 里的 `tai_talea_bootstrap/` 停在
> 2026-09-30，白名单只有 19 项。所以**改仓库不重打镜像 = 白改**：首次真实
> `/bootstrap/start` 直接 `HTTP 422 extra argument '--ep-size' is not allowed`。
> 已重建镜像为 `library/talea-worker:v0.2`。排查与修复全过程见
> `docs/worker-host-access-and-allowlist-2026-10-10.md` §4。

### M3 + C2 Router 二进制与权重挂载

- Router 二进制不用额外搬运：sglang 0.5.20 镜像里自带 `python3 -m sglang_router.launch_router`，
  `talea-router.yaml` 改用镜像内命令即可（聚合模式、不传 --prefill/--decode，
  让控制面 `POST /workers` 动态注册）。
- **清单缺权重 hostPath**：Router 的 tokenizer 必须本地加载，air-gap 下不挂
  `/apps/models` 会在第一个请求上死（今天实测踩过：`glm53-pd-router.yaml` 修过一次）。
- 端口约定要对齐：模板 :30000，控制面 `router.url` 相应写 `http://<节点IP>:30000`。
工作量：0.5 天。

### C1 动态注册 readiness 实测（验证，非开发）—— 2026-10-10 探明根因

今晚的 Router 是**静态** `--prefill/--decode` 起的，没走 talea 的注册/摘流通道。
历史上 pip sglang-router 0.3.2 对动态注册 worker readiness 一律 404（capability-gap §9）、
R13 异步注册暂态 404 引发误摘流——smg fork 后来重构成 workflow 注册 + state 层，
**但没在真机验过动态注册后的 readiness 可见性**。这是 talea 摘流语义的前提。

**2026-10-10 清卡后实测，C1 当场复现，且根因不是代码：**

集群里跑的 Router 是**旧版 Python `sglang_router`**（`python3 -m sglang_router.launch_router`，
即 23 h 前手工 1P1D 留下的 Job `glm53-pd-router`）。探针实测：

| 请求 | 结果 |
| --- | --- |
| `GET /workers` | 200（注册/列表在） |
| `GET /state/workers` | **404** ← `readiness-v1` 的「就绪面」不存在 |

`/state/workers` 是 **Rust `sglang-router`（crate `smg` / `sgl-model-gateway`）** 的管理面
（`sglang-router/src/server.rs:860`），Python 版没有。而控制面**只支持 `readiness-v1`**
（`routeradapter/client.go:94` 硬校验），所以：

```
WARN  planner assign failed  "router readiness failed: router worker not found: 9d717cde-…"
ERROR reconcile instance failed "router readiness failed during reconcile: router worker not found: …"
```

注册能过、就绪必挂 → `SERVING` 永远到不了，drain 的负载探针也读不到负载
（回收会走 `drain_timeout` 强制停，见 `docs/a2-real-run-and-card-clear-2026-10-10.md` §2.5）。

**结论：C1 的前置动作 = 把 Rust `sglang-router` 部署进集群**（构建路线三选一，
见该文档 §3.4）。这不是改控制面能绕开的：就绪是 talea 唯一的摘流原语
（`lifecycle.go:662`「Close readiness first: this is the only supported drain primitive」）。

### D1/D2 端到端验收（2–3 人日）

按「无 AI agent 合同」执行，全部通过控制面 API 驱动：

1. M1 就绪后 `POST /v1/capacity/console/provision {count:2}`；
2. planner 1:1 分出 prefill/decode，bootstrap 拉起，注册进 Router 两池；
3. Router :30000 真实推理 1P1D 全链路；
4. `talea benchmark`（GSM8K）跑基线，与今天 bench_serving 数据互校；
5. 摘流/归还/控制面重启恢复各一遍（T4/T5 场景）。

## 4. 已知运行期风险（带入自动化后的观察项）

- **闲置重连罚时**：PD 链路空闲数小时后首批请求 TTFT ~30s（今天实测），热态 0.85s。
  自动化后健康检查/验收脚本要避开「冷第一发」，或加 keepalive 预热。
- **h100-02/03 未授权**：四节点 2P2D 扩容被平台权限卡住，2 台只能维持 1P1D。
- **占卡容器**：平台 vLLM 复现容器（moby ns）若复活会抢 h100-04 的卡，
  TP 内存均衡检查会 fatal——控制面的 start 失败诊断信息要能指向这个原因。
- h100-01 hostPath `/` 只读、写宿主必须 chroot——只影响投放操作，不影响运行期。

## 5. 排期汇总

| 项 | 工作量 | 依赖 | 状态 |
| --- | --- | --- | --- |
| M1 控制面进集群 | 1 人日 | — | ✅ 2026-10-09 |
| M2 worker 镜像供给 | 0.5 人日 | ecis registry | ✅ 2026-10-09（自足镜像 + kubelet 自动拉取） |
| A1 adapter 宿主开关 | 0.5 人日 | 无 | ✅ 2026-10-10（集群验收过：Pod IP = 宿主 IP） |
| A2 按节点/角色配置模型 | 1 人日 | A1 | ✅ 2026-10-10（代码 + 模板 + 测试 + **真机端到端**，见 `a2-acceptance-2026-10-10.md`、`a2-real-run-and-card-clear-2026-10-10.md`） |
| B1 白名单 + 配置修正 | 0.5 人日 | 无 | ✅ 2026-10-10（白名单 19→35；镜像需重打，见附） |
| M3+C2 Router 模板修正 | 0.5 人日 | 无 | 待做 |
| C1 动态注册 readiness 实测 | 0.5 人日 | **Rust `sglang-router` 部署进集群**（新前置，见 §3） | 阻塞：现场已复现，根因是路由器类型 |
| D1/D2 端到端验收 | 2–3 人日 | 全部 | 待做 |

**剩余约 2.5–3.5 人日**到「一句话 provision → 自动 1P1D SERVING → 可摘流回收」，
其中新增的 0.5–1 人日是**部署 Rust `sglang-router`**（C1 的前提，2026-10-10 新暴露），
不含平台侧（SA 授权、h100-02/03、h100-01 的 ecis hosts 映射）的等待。

M1 与 M2 已于 2026-10-09 晚完成。两条链路都换成了镜像路线：
供应从「每节点手工 `ctr export/import` 搬 13 GB」变成「push 一次、kubelet 自动拉取」，
控制面也从「hostPath 投放二进制」升级成「小镜像 + kubelet 自动拉取」。

A1、B1、A2 的三份代码都已完成，**三者的真机验收也都在 2026-10-10 凌晨走完**：

* A1：`provision` API 建出的 worker Pod 上 `podIP == hostIP`；
* B1：失败点从「参数被拒（422）」推进到「sglang 起不来」，证明参数确实进了 sglang；
* A2：同一份配置下 prefill 与 decode **各自**产出一条 `dispatching start` 日志，
  两条命令行的差异就是设计好的差异（`--max-running-requests` / `--cuda-graph-max-bs-decode`
  只在 decode，设备表两侧不同且 prefill 侧不含坏卡 `mlx5_6`），节点级 `NCCL_IB_HCA`/`NCCL_IB_TC`
  也确实进了 Pod env。

**A2 的验收不依赖 GPU 空闲**，因为观察点是「控制面下发的命令行」而不是「SGLang 成功服务」。
为了让 prefill 和 decode 同时存在，验收用了一份**只改两行**的配置副本
（`host_network: false`、`gpu_count: 4`），原因见观察项 5、6；现场已还原为生产值。

**2026-10-10 凌晨：清卡已做，A2 的真机端到端也补上了。**

用户授权后停掉了 h100-04 上占卡的第三方容器 `sgl_0521`（restart policy `no`，
`docker stop` 保留容器，可 `docker start` 回滚）→ 8 张卡归零。
随后用**生产配置**发一次 provision，worker 走完了整条链路：

* 控制面 `container enrolled` → `instance prepared` → `dispatching start`（role=decode，带完整 argv）；
* 容器内 `/bootstrap/status` 回出**真实执行的命令行**，`environment` 四元组与 `image_profile` 一致（未 exit 29）；
* `/bootstrap/health` 从 `degraded` 翻到 **`ok` / "sglang answered the health probe"**；
* 宿主 `nvidia-smi` 8 张卡各 **70.5 GiB**，即 `--mem-fraction-static=0.85` 的 KV 池；
* reclaim 也顺带验了（`service drained forced=true exit_code=0` → `instance released`）。

完整证据链见 **`docs/a2-real-run-and-card-clear-2026-10-10.md`**。

**现在唯一的硬阻塞是 C1 的载体**：集群里的 Router 是旧版 Python `sglang_router`，
`GET /state/workers` 回 404 —— 而 `readiness-v1` 是控制面唯一支持的模式。
要跑 C1，必须先把 Rust `sglang-router` 部署进集群。
所以顺序是：**部署 Rust router（C1）→ h100-01 的 ecis hosts（平台）→ 1P1D 端到端 → D1/D2**。

### 附：本轮新增的观察项

1. **镜像里的 bundle 是快照，改了 `roles.py` 必须重打镜像**（tag 也要换，因为
   `imagePullPolicy: Always` 只保证拉最新，不保证 tag 内容不变）。B1 首次真实启动就撞在这上面。
2. **worker 的 start 路径此前从未被真正走通**。M2 验的是「镜像能拉起来 + bootstrap 答健康检查」，
   不是「能 start」。这条盲区现在补上了：`/bootstrap/start` 会回 422 并带明确的 `detail`，
   控制面把它原样写进 `service_start_failed` 告警——排查入口是这条告警，不是 kubelet。
3. **控制面 Pod 重建会换 IP**，写死 IP 的探针会拿到 `HTTP 000`（连接失败），别误判成鉴权问题。
4. ⛔ **h100-04 的 8 张卡已被一个非 k8s 的 Docker 容器占满**（容器 `sgl_0521`，
   镜像 `vllmascend-…/dev/sglang:v0.5.21-cu130`，上游 v0.5.21，不是内部 fork）。
   每卡 82 GB 里用掉 63.5 GiB，只剩约 18 GiB，而 `--mem-fraction-static=0.85` 要约 68 GiB
   （A2 之前模板里的 0.60 要约 48 GiB，同样不够）。
   症状是 worker 侧的 `sglang exited unexpectedly with code -9`，看上去像代码问题。
   **这是 A2（验收）/ C1 / D1 的硬前置**：不清掉它，控制面建多少 worker 都起不来。
   **2026-10-10 凌晨用户授权后已清**：`docker stop`（restart policy 为 `no`，容器保留、
   `docker start sgl_0521` 可回滚）→ 8 张卡归零。识别与处置手法见
   `docs/a2-real-run-and-card-clear-2026-10-10.md` §1。
   **新出现的外部风险同类**：h100-04 上还有 `eval-chy` / `glm-chy` / `eval-wzj` 三个 vllm
   容器在跑（当前不占 GPU），若它们恢复占卡，worker 会再次起不来。

5. **单个实例永远是 decode**。`planner.Plan` 的目标数是 `TargetPrefill = floor(total * ratio)`，
   而 `total >= 2` 时才有 `max(1, min(total-1, ·))`。所以只有一个可用实例时它必然被分成
   decode —— 想「只验 prefill」在现有 planner 下做不到。`gears` 也只接受
   `1:1 / 1:2 / 2:1`，没有 `1:0` 或 `0:1`（实测回 `invalid_gear`）。
   推论：**验收角色差异必须让两个实例同时存在**。

6. **hostNetwork 把「一节点一个 worker」变成硬约束**。endpoint 取的是 `pod.Status.PodIP`；
   开了 hostNetwork，同节点所有 worker 的 Pod IP 都等于节点 IP，于是第二个实例 enrol 直接被拒：
   `endpoint already belongs to another active node`。所以 `nodes` 列表的轮转不是可选优化，
   而是**唯一**能让两个 worker 分到不同地址的手段。同节点放两个 worker 需要临时关掉
   hostNetwork，而那时 `gpu_count: 8` 又会让第二个 Pod 永远调度不上——两条一起才成立。

7. **控制面从 v0.3 起记录它下发的 `extra_args`**（`internal/controller/lifecycle.go`）。
   在此之前两侧都不留痕：控制面不看 202 响应体（`launcher.Client.Start` 传 `destination=nil`），
   而渲染好的命令行恰好只在那个响应体的 `detail` 里，加上 bootstrap 自己的
   `/bootstrap/status` history 又随 worker 一起消失。这行日志是 A2 的验收观察窗，
   也是以后 PD 两侧不对称时的第一排查入口。

