# A2：角色级与节点级参数覆盖机制（2026-10-10）

> 前置：A1（宿主访问开关）与 B1（白名单）见 `worker-host-access-and-allowlist-2026-10-10.md`。
> 本文只讲 A2：**让同一份控制面配置能同时描述 prefill 与 decode 两个角色、以及硬件不同的两台机器。**

## 1. 结论

| 项 | 状态 |
| --- | --- |
| 角色级参数覆盖（`extra_sglang_args_by_role`） | ✅ 代码 + 单测 + **真机** |
| 节点级环境覆盖（`kubernetes.nodes[].env`） | ✅ 代码 + 单测 + **真机** |
| 节点轮转（一次 provision N 个 → 分散到 N 台机） | ✅ 代码 + 单测（含并发） |
| 部署模板对齐真机验证值 | ✅ 模板 + 契约测试 |
| Go / Python 全量测试 | ✅ 全绿 |
| **真机验收（同配置下 prefill 与 decode 各收到一条命令行）** | ✅ **2026-10-10**，原始日志见 `a2-acceptance-2026-10-10.md` |

**验收结论**：同一份配置、同一次 `provision count=2` 下，prefill 与 decode 各自产出一条
`dispatching start` 日志；两条命令行的差集恰好是第 2 节列出的差异，共享段逐字相同、
且每个旗标只出现一次。

这次验收**不依赖 GPU 空闲** —— 验的是「控制面下发给 SGLang 的命令行」，
而它在调用 `Start` 之前就已落进控制面日志，与 SGLang 能否成功启动无关。

## 2. 为什么需要「覆盖」，而不是一份共享列表

B1 之后，`extra_sglang_args` 是一份共享列表，发给 `/bootstrap/start` 时对两个角色完全一样。这在真机上行不通，有两个独立的原因：

**原因一：两个角色的参数本来就不同。** 从 2026-10-08/09 验证过的 1P1D manifest
（`.workbuddy/tmp/alayanew/glm53-pd-{prefill,decode}.yaml`）逐条对出来：

| 参数 | prefill (h100-01) | decode (h100-04) |
| --- | --- | --- |
| `--disaggregation-ib-device` | `mlx5_2,mlx5_3,mlx5_4,mlx5_5,mlx5_7,mlx5_8,mlx5_9,mlx5_10` | `mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_5,mlx5_6,mlx5_7,mlx5_8` |
| `--max-running-requests` | — | `128` |
| `--cuda-graph-max-bs-decode` | — | `128` |
| `--chunked-prefill-size` | `512` | `512` |
| `--mem-fraction-static` | `0.85` | `0.85` |

`--disaggregation-ib-device` 的差异是已知的硬故障：**h100-01 上名为 `mlx5_6` 的卡链路状态 Active，但流量到不了对端**，
留在列表里就是第一次 KV 传输挂死，且没有任何错误信息可读。

**原因二：两台机器的硬件不同。** `NCCL_IB_HCA` / `NCCL_IB_TC` 在第一次集合通信之前就被读取，
而 h100-01 的 IB 口名从 `mlx5_2` 起、h100-04 从 `mlx5_0` 起。把 decode 的列表抄给 prefill，
等于点名了一批不存在的设备。

这两件事的**作用域不同**，所以机制也分成两层：

- 与角色绑定的 → 角色级（start 时才知道角色，此时下发）
- 与机器绑定的 → 节点级（env 在创建时就固定，改不了）

## 3. 角色级：`extra_sglang_args_by_role`

### 3.1 配置形状

```yaml
controller:
  extra_sglang_args:            # 共享：两个角色都要
    - --tp-size=8
    - --chunked-prefill-size=512
    - --mem-fraction-static=0.85
    # ...
  extra_sglang_args_by_role:    # 例外：只有某个角色要
    prefill:
      - --disaggregation-ib-device=mlx5_2,mlx5_3,mlx5_4,mlx5_5,mlx5_7,mlx5_8,mlx5_9,mlx5_10
    decode:
      - --max-running-requests=128
      - --cuda-graph-max-bs-decode=128
      - --disaggregation-ib-device=mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_5,mlx5_6,mlx5_7,mlx5_8
```

### 3.2 合并语义：按「旗标」覆盖，不是追加

`internal/controller/sglang_args.go` 的 `mergeSGLangArgs(base, override)`：

- 逐个**旗标**处理，后面的覆盖前面的**取值**，且**保留首次出现的位置**
- 结果里每个旗标**最多出现一次**
- `--tp-size=8` 与 `--tp-size 8` 被识别为同一个旗标

为什么不是简单追加？**SGLang 用 argparse 解析命令行，重复旗标合法、只取最后一个。**
追加式覆盖会生成一个「同时带着两个互相矛盾取值、语义取决于参数顺序」的命令行——
它能跑，但渲染出来的命令没法一眼看出实际生效的是哪个值，也没法 diff。
按旗标合并之后，prefill 和 decode 的命令行可以逐行对照。

### 3.3 校验

`config.Validate` 拒绝：
- `extra_sglang_args_by_role` 里出现非 `prefill`/`decode` 的 key
- 任何空白参数项

## 4. 节点级：`kubernetes.nodes`

### 4.1 配置形状

```yaml
partners:
  - id: cmcc-k8s
    adapter: kubernetes
    kubernetes:
      node_selector:                  # nodes 为空时的回退
        kubernetes.io/hostname: h100-04
      nodes:
        - host: h100-04
          env:
            NCCL_IB_TC: "106"
            NCCL_IB_HCA: "mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_5,mlx5_6,mlx5_7,mlx5_8"
        - host: h100-01                # 无自有 env，与验证过的 prefill manifest 一致
```

### 4.2 轮转语义

`Adapter.nextNode()` 用一个 `atomic.Uint64` 游标，每创建一个 Pod 前进一格：

- `nodes` **为空** → 完全退回旧行为（`node_selector` + 单一 env），单机部署零改动
- `nodes` **非空** → 第 1 个 worker 去第 1 台，第 2 个去第 2 台，第 3 个绕回第 1 台

轮转是「两台机器上跑 1P1D」这件事能成立的前提——没有它，两个 worker 都会声明同一台的 8 张卡。
游标是原子的：provision 请求从不同 goroutine 进来，丢一次更新就是把两个 worker 叠到同一台机上。

Pod 上会打一个 `tai-talea.node: <host>` 标签，运维不用翻 spec 就能看出谁落在哪台机。

### 4.3 env 优先级与选择器

环境变量三层，**越具体越优先**：

```
固定注入（token/port/profile） < partner 级 env < 节点 env
```

节点未配 `node_selector` 时按 `kubernetes.io/hostname: <host>` 钉住——
这是租户唯一能依赖的节点标签，因为它由 kubelet 自己打；自定义 pool 标签需要读 Node 对象才能验证，
而这个集群的租户 token 连 `get nodes` 都是 Forbidden。

### 4.4 校验

`config.Validate` 与 `kubernetes.New` 都会拒绝：空 host、重复 host、非法的环境变量名。
重复 host 尤其值得拦：它会安静地把两个 worker 叠到同一台机上，另一台闲着。

### 4.5 一个必须写明的边界：`--disaggregation-ib-device` 的作用域

这个取值看起来是「按角色」的，**实际上是「按机器」的**：
`mlx5_6` 是 h100-01 上那张链路 Active 但不过流量的卡，而 h100-04 上同样叫 `mlx5_6` 的卡是好的。
两台机的卡名空间重叠，物理设备不同。

**本实现已按机器取值**（2026-10-12 落地，见下），不再依赖「角色与节点一一对应」。

#### 为什么必须这样

角色和机器是**两条互不知情的决策**：

- 角色由 planner 定（`rebalance.go` Phase 1/2），
- 机器由 partner 的轮转定（`provision/kubernetes` 的 `NodeProfile` 轮转）。

把机器属性放在角色层，就等于让其中一条决策去猜另一条。
实测的错配路径很具体：`rebalance.go` 把 idle 按 **ID 字典序**排序
（`sort.Slice(input.Idle, ID <)`），而 `planner.go` Phase 1 把 prefill 补给 `idle[0]`；
点号 `.`(0x2E) < 连字符 `-`(0x2D)，所以无后缀的 `…172229` 字典序更小——
**两实例同时 idle 时 prefill 落到 h100-04**，然后被喂 h100-01 的 HCA 名单，SGLang 启动即退出。
失败时控制面所有字段都正常，因为它们在控制面这一侧本来就是对的。

#### 落地形态：三层合并

| 层 | 配置位置 | 语义 |
| --- | --- | --- |
| 1 | `controller.extra_sglang_args` | 两个角色、每台机都接受的 |
| 2 | `controller.extra_sglang_args_by_role.<role>` | 角色之间不同的（`chunked-prefill-size` 一类） |
| 3 | **`kubernetes.nodes[].extra_sglang_args`** | **只有这台机接受的**（`--disaggregation-ib-device`） |

`mergeSGLangArgs` 按 flag 名（`argFlag()`）做 key，同 flag 后写**替换同位置**而不是追加，
所以第 3 层是「覆盖」而不是「多一份」。链子：
`NodeProfile.ExtraSGLangArgs`（config）→ `domain.InstanceSpec.SGLangArgsOverride`（随 spec 持久化）
→ `lifecycle.startRequest` 第三层 merge。

#### 机器怎么被认出来（以及为什么不能信轮转游标）

适配器从**已调度 Pod 读回 `pod.Spec.NodeName`**（`kubernetes/client.go` 只读、从不写），
再用 `nodeProfileFor(host)` 查 `nodes[]`。不走轮转游标，原因有二：
控制面重启会把游标重置，而 `adoptVerifiedEndpoint` 这条纳管路径从不推进游标——
两者都会让「以为落在 A 机」和「实际落在 B 机」分家。

#### 关于"要不要迁移 schema"

当初推迟此修复的第二条理由是「需要给 `domain.Instance` 加字段并迁移 SQLite schema」。
**这条不成立**：`InstanceSpec` 以 JSON 存进 `store/sqlite.go` 的 `spec_json` 列
（`:637` / `:673`），加字段对持久化透明，无需迁移。

#### 验证

- 单测：`kubernetes::TestMachineArgsFollowTheScheduledNode`（两机各回本机名单、
  未调度/未列节点返回 nil、无 `nodes` 时不发明 override）、
  `kubernetes::TestNodeRotationStillDecidesPlacement`、
  `controller::TestDeploymentTemplateSplitsItsArgumentsIntoThreeLayers`
- 本地渲染：`h100-04 as prefill/decode → mlx5_0,1,2,3,5,6,7,8`；
  `h100-01 as prefill/decode → mlx5_2,3,4,5,7,8,9,10`（名单只跟机器走）
- **真机**：`-172229`（h100-04）被 planner 从 decode 改成 **prefill** 时，
  日志下发的仍是 `--disaggregation-ib-device=mlx5_0,mlx5_1,mlx5_2,mlx5_3,mlx5_5,mlx5_6,mlx5_7,mlx5_8`
  （h100-04 自己的名单），**角色换了、名单没换**——这正是本轮要达成的性质

> ⚠️ 模板里 `nodes[].extra_sglang_args` 仍需与本机实测对齐；
> 换机器/换卡后要重新实测（`ibstat`/`ibv_devinfo`），不要照抄另一台机的名单。

## 5. 模板改了什么

`deploy/kubernetes/tai-talea.yaml`：

1. `extra_sglang_args` 收敛为「两个角色都要的」，`chunked-prefill-size=512` 归入共享
2. 新增 `extra_sglang_args_by_role`，取值逐条抄自验证过的 1P1D manifest
3. **`--mem-fraction-static` 从 `0.60` 改为 `0.85`**：
   0.85 是 PD 布局下的验证值。0.60 属于聚合布局（+ 投机解码）——
   改错的表现是 warmup 阶段的 illegal memory access，而不是 OOM，很容易误判成代码问题
4. 新增 `nodes`：h100-04（带 NCCL 环境）+ h100-01（无自有环境）
5. partner 级 `env` 加 `GLOO_SOCKET_IFNAME` / `NCCL_SOCKET_IFNAME=bond1.1081`：
   两台机都走同一个 bond 口。不钉住时 NCCL/Gloo 自己猜，而这台机上有一个管理 bond、
   两个 IPoIB 接口、八个 RDMA 设备——猜错的唯一表现是集合通信里挂死

> ⚠️ h100-01 已列进 `nodes`，但**目前不可用**：它的 `/etc/hosts` 还缺 ecis registry 条目，
> 调度过去的 Pod 拉不到镜像。在此之前，第 2 个 worker 会落到 h100-01 并停在 ImagePullBackOff。

## 6. 测试

| 测试 | 覆盖 |
| --- | --- |
| `controller::TestMergeSGLangArgs*`（6 项） | 追加 / 覆盖 / 位置保持 / `=` 与空格等价 / 空安全 / 不 alias 共享切片 / 去重 / 空串 |
| `controller::TestDeploymentTemplateSplitsIntoTwoDisjointRoles` | 用**真实模板**跑合并：每个旗标只出现一次、prefill 的 device list 不含 `mlx5_6`、decode 独有边界参数不泄漏到 prefill |
| `kubernetes::TestNodeRotationSpreadsWorkersAndMergesEnv` | 轮转 + 绕回 + 三层 env 优先级 + 无 nodes 时行为不变 |
| `kubernetes::TestNodeSelectorOverridesThePartnerSelector` | 显式 selector 优先于 hostname 钉法 |
| `kubernetes::TestNodeRotationIsSafeUnderConcurrency` | 40 个并发创建，两台机各拿一半 |
| `kubernetes::TestNewRejectsBrokenNodeProfiles` | 空 host / 重复 host |
| `config::TestKubernetesConfigurationIsValid`（扩展） | 模板里两台机都在、h100-04 有 NCCL_IB_HCA 而 h100-01 没有、共享接口被钉住 |
| `config::TestValidationRejectsIncompleteConfiguration`（+6 项） | 节点与角色参数的负面用例 |
| `bootstrap::test_every_flag_in_the_deployment_template_is_allowed` | **从模板正则提取全部旗标，逐个过白名单**——把「模板改了但白名单没跟上」变成编译期一样的失败 |

最后一项是这轮最有价值的防护：B1 的教训正是「模板/配置发了白名单没有的旗标，
失败要到 worker 已经起来之后才以 422 现形」。

`go test ./...` 全绿；bootstrap 70 项通过（2 项在非 Linux 上按设计跳过）。

## 7. 真机验收（2026-10-10 完成）

走完了，**而且没有被清卡挡住** —— 因为验的是控制面下发的命令行，不是 SGLang 成功服务。
完整步骤、原始日志与证据在 `a2-acceptance-2026-10-10.md`；这里只留要点：

1. 交叉编译 → 推 `library/tai-talea:v0.3`（manifest `sha256:9090b1c1a193…`）；
   控制面新增一行 `dispatching start` 日志 —— 在此之前，下发的参数两侧都没有留痕。
2. 投放 v0.3 配置并重建控制面 Pod。
3. `provision count=2` → planner 一个 prefill、一个 decode。
4. 控制面日志给出两条 `dispatching start`，`extra_args` 正是第 2 节列出的差异。
5. 两个 worker Pod 的 env 里 `NCCL_IB_HCA` / `NCCL_IB_TC` 来自 `nodes[].env`，
   与 partner env 并存，落点标签 `tai-talea.node` 也在。

为了两个 worker 能并存，验收配置临时放开了两处（`host_network: false`、`gpu_count: 4`），
原因见验收文档 §2.3；其余与生产一致，现场已还原。

**仍未验的一步**：参数到了 SGLang 手里之后，PD 两端能不能用这份设备表把 KV 传起来。
它卡在同一个外部因素上：

```
h100-04 八张卡：每张 63.5 GiB / 81,559 MiB 已占用，只剩 ~18 GiB
worker 需要 --mem-fraction-static=0.85 → 每卡约 68 GiB  → 必然 code -9
占用者：非 k8s 的 Docker 容器 sgl_0521
```

同一台机上另有 `eval-chy`、`glm-chy` 两个同样 running 的 vllm 容器，都不是本项目负载。
**全程只做只读识别，未 stop / kill 任何东西。**

## 8. 顺带确认的事实

- 单副本控制面、绑 h100-04；`tale-worker-service` 仍在跑（跳板用途）
- 集群里残留两个 22 小时前的 `Completed` PD Pod 和一个 `Running` 的 `glm53-pd-router-lksx7`，非本轮产生
- `kubectl get events` 依然 Forbidden，观察窗口仍只有 kubelet / 控制面日志
