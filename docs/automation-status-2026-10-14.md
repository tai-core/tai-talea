# 自动化部署：现在到底自动到哪一步（2026-10-14）

> 回答一个问题：**「一条请求下去，机器会自己变成在服务的推理节点吗？」**
> 结论分三层，**前两层是自动的，第三层（要不要机器）代码写完但两台平台都关着，第四层（坏了自愈）完全没有。**

## 1. 一句话

**从「机器已经在手」到「在服务」是全自动的，并且在 nightshift 上真机闭环验收过；
从「要不要机器」到「自动去下单」代码已经写完但没打开；
从「坏了」到「自己好」——不自动，今天正好栽在这里。**

## 2. 四层能力对照

| 层 | 能力 | 代码 | 真机验证 |
| --- | --- | --- | --- |
| **① 已就绪容器 → 在服务的节点** | 分角色 → 按角色下发 argv → 起 SGLang → 注册 Router → readiness → SERVING | ✅ | ✅ **nightshift 已闭环** |
| **② 裸容器 → 已就绪** | SSH 上线：root/系统/Python/GPU/主机密钥自检 → SFTP 分发 wheel+model+bootstrap 并核 SHA256 → apt → venv → 离线 pip → CUDA/NIXL 自检 → 装 bootstrap → 纳管 | ✅ | ⚠️ **九章需要它，但没有成功记录** |
| **③ 要不要机器：自动加/减容量** | `planner.mode: auto` + `capacity_actions`（PROVISION/RELEASE） | ✅ | ❌ **两台平台都关着** |
| **④ 坏了自愈** | —— | ❌ | ❌ **今天证明不成立** |

## 3. ① 的证据：D1（2026-10-11，nightshift）

一条请求，两台机器，14 分 20 秒，**全程无人工干预**：

```
POST /provision {count:2}
  → 建两个 Pod（分落 h100-04 / h100-01）
  → planner 按 gear=1:1 分出 1 prefill + 1 decode
  → /bootstrap/start 下发**各自不同**的 argv（--disaggregation-mode + --disaggregation-ib-device）
  → SGLang 起来 → POST /workers 注册 → 两侧 SERVING
  → Router 双池 prefill_count:1 / decode_count:1
  → /readiness 200 → 经 Router 真实推理 200（1.04s / 0.71s，答案正确）
```

端点：`docs/d1-acceptance-2026-10-11.md`。2026-10-12 复验一次（`d1-revalidation-…`）。
摘流/恢复（D2）2026-10-13 也过了：decode `DRAINING` → 3 分钟后回 `SERVING`、`healthy=2`。

**这一层是"指挥官"能力的完整形态**：给什么形状的机器，它都能分好角色、用对的参数拉起来、接上 Router、并干净地摘流归还。

## 4. ② 的现状：九章缺这一层，而且没跑通

nightshift **不需要**这一层 —— 它的 partner 是 `SelfProvisioningAdapter`，"建 Pod 即铺装"，
镜像里已经烤好了 bootstrap 和 SGLang（`imagePullPolicy: Never`，镜像预先 ctr import 进节点）。

九章**需要**这一层：平台给的是一台裸的 Ubuntu 容器，控制面得自己 SSH 进去装。
代码在 `internal/onboarding` + `provision/ssh_install.py`，配置在 `onboarding:` 段。

**九章这条链还没有成功记录**（我们记录到的作业历史里多数 FAILED），线上那台在服务的
`glm53-prefill` 当初是**手工铺的**（宿主 `/opt/tai-talea` 布局）。
⇒ 所以 ② 是**写完了但没在九章验收过**的状态。
（待复核：容器现已停止，作业列表暂时查不到。）

## 5. ③ 的现状：代码写完，开关没打开

`planner.mode` 默认 **`fixed`**，`capacity_actions` 默认 **`false`**，
而且 `Validate()` 强制 `capacity_actions` 只能在 `mode: auto` 下开。

| 平台 | 线上实际值 |
| --- | --- |
| nightshift | `mode: fixed` / `capacity_actions: false` |
| 九章 | 同上（配置里没有 `planner` 段，取默认） |

⇒ **"按线上 PD 压力自动加机器/还机器"这件事，代码已经做完，但没有在任何一台平台上开过。**
而且它依赖 ① 的观测（`Snapshot.Complete` 为真）；九章上 `--enable-metrics` 是 10-13 才补进配置的，
还没经过一次真实节点验证。

## 6. ④ 的现状：不自动 —— 今天正好撞上

两台平台同时暴露同一类问题（详见各自的停摆记录）：

| 症状 | 为什么自愈不了 |
| --- | --- |
| `start_attempts` 烧到上限 | `reconcile` 对烧完的实例**直接放弃**，`rebalance` 又**看不见 FAILED 实例** ⇒ 需要人重置 |
| LOST / 僵尸父进程 | 父进程活着、8 个 scheduler 全是 `<defunct>`、端口无监听；不清掉，下次启动撞同一个端口 |
| 机器被第三方占着 | planner 只看自己的成员表，看不见宿主机上的外来 docker 容器 |
| 外部把容器停了 | **我们的代码只有 `release`，没有 `start`/`stop`** ⇒ 平台把容器停下后，我们连"再启动"这个动作都没有 |

⇒ **部署自动 = 成立；运维自动 = 不成立。**

---

## 7. 九章的状态变化（2026-10-13 22:00，外部动作）

两个实例都被**停**了（不是释放）：

| 实例 | 状态 | 起 | 停 | 释放 |
| --- | --- | --- | --- | --- |
| `glm53-prefill` | **STOPPED** | 09-30 11:50:44 | **10-13 22:00:46** | 无 |
| `glm53-pd2` | **STOPPED** | **10-13 22:01:13** | 10-13 22:04:25 | 无 |

两点值得记：

1. **`glm53-pd2` 真的被调度到了** —— `createTime 10:33:29` 排队，**`startTime 22:01:13` 拿到容器**，
   即排队 **11 小时 28 分**后成功。这印证了"排队不丢位次"的修法是对的：
   换成旧行为，它会在 5h30m（`wait_timeout`）被我们自己释放掉，根本活不到 22:01。
2. **不是我们停的。** 我们的代码只有 `release`，没有 stop；且 `autoStopEnable` / `autoReleaseEnable`
   都是 `false`，`releaseTime` 为空 ⇒ 是平台/控制台上的人干的。**这也暴露了一个能力缺口：
   我们没有任何"启动/停止实例"的动词，只有一个不可逆的"释放"。**

控制面跑在 `glm53-prefill` 里面 ⇒ **九章侧现在整体离线**。

## 8. 要真正叫"能自动部署"，还差的三件事

| # | 缺什么 | 落点 |
| --- | --- | --- |
| 1 | 九章上把 ② 跑通一次（裸容器 → 自动装好 bootstrap → `MANAGED`） | `internal/onboarding` |
| 2 | 开 ③：先把 nightshift 的 `mode` 改 `auto`（`capacity_actions` 可继续关），观察档位真的会动 | `planner.mode` |
| 3 | 给 ④ 补最小自愈：`start_attempts` 用尽后可重试、FAILED 实例对 rebalance 可见、LOST 记录可清理 | `internal/controller` |

另加一条运维必备：**`start`/`stop` 两个动词**（不只是 `release`），否则平台一停我们就只能干看着。
