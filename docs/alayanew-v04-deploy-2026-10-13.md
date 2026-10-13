# 九章智算云：v0.4 控制面部署与指标闸门（2026-10-13）

## 0. 结论速览

| # | 项 | 结果 |
| --- | --- | --- |
| 1 | 控制面升级 | `fix4-provision-budget` → **`0.4.0 (ec5fb44+provision-queue)`**，重启接管 `checked=6 lost=0 reregistered=0` |
| 2 | 影响面 | 注册表 / Router / **worker 进程全部未动**（router pid 242331、sglang pid 236368 前后一致） |
| 3 | 回滚件 | `/root/tai-talea.bak-20261013-102126`（旧二进制）、`/etc/tai-talea/tai-talea.yaml.bak-20261013-102521`（旧配置） |
| 4 | **新发现** | **`--enable-metrics` 从未下发** ⇒ `/metrics` 404 ⇒ 弹性在九章不可能成立 |
| 5 | 处置 | 该 flag 已加入线上 `controller.extra_sglang_args`；**只对新铺装的节点生效** |

---

## 1. 这一版带去了什么

27 个提交，其中与九章直接相关的：

- **#77 prefill 指标名兼容**（`num_prefill_prealloc_queue_reqs` ← `num_prefill_bootstrap_queue_reqs`）
- **#78 planner 角色下限保护**（某角色只剩 1 个实例时禁止 rebalance 走）
- **#79 planner 消费 `LoadSnapshot`**（`planner.mode: auto`）
- **#80 PROVISION / RELEASE 两个容量动词**（`capacity_actions`，默认关闭）
- **按需创建的队列语义**（排队不丢位次）与 **`212035` 自动换名**，见
  `provision-queue-and-names-2026-10-13.md`

新暴露的观测面：`/v1/capacity/planner` 回 `mode` / `effective_gear`，
`/v1/capacity/console` 回 `planner_mode` / `capacity_actions`，启动日志打
`planner configuration mode=… gear=… capacity_actions=… provision_timeout=…`。

线上配置没有新的 `planner` 键，取默认值 `mode: fixed` / `capacity_actions: false` /
`auto_dead_band: 0.2` / `auto_release_below: 0.1` —— **与升级前行为等价**，所以本次只换二进制。

---

## 2. ⚠️ 指标闸门：`--enable-metrics` 从来没传过

升级后 `console/telemetry` 里唯一的 decode 节点是 `available: false`，
错误文案「指标不可用（请检查 --enable-metrics）」。逐层查：

| 探测 | 结果 |
| --- | --- |
| `GET http://<service_endpoint>/metrics` | **404**（22 字节） |
| `GET /get_server_info` → `enable_metrics` | **`False`** |
| `sglang:*` gauge 数量 | **0** |

SGLang 0.5.20 上 metrics 是**选择性开启**的，而我们的 `extra_sglang_args` 里从来没有这个 flag。
后果是完整的一条链：`/metrics` 404 ⇒ `Node.Available=false` ⇒
`Snapshot.Complete=false`（`advice: 等待 P/D 完整指标`）⇒ planner 的负载输入恒为空。

⇒ **#77 是必要但不充分的**：它修的是「gauge 改名了读不到」，而九章上连 gauge 都没有导出。
两者叠加才是「弹性在九章不成立」的完整原因。

### 处置

把 `--enable-metrics` 加到 **`controller.extra_sglang_args`（共享层）** ——
两个角色都要（collector 在 prefill 读 bootstrap 队列、在 decode 读 prefill 队列），
而 bootstrap 的 `ALLOWED_EXTRA_FLAGS`（`bootstrap/tai_talea_bootstrap/roles.py`）本来就已经放行它。

校验用了**节点真正会跑的那份活 bootstrap**，而不是本地读仓库模板：

```
PYTHONPATH=/opt/tai-talea python3 -c from tai_talea_bootstrap.roles import validate_extra_args
  共享 18 条 → prefill 1 条 / decode 3 条 → 31 个 argv token，--enable-metrics 在列
```

随后 v0.4 二进制的 `-check-config` 通过、控制面重启接管干净（`checked=6 lost=0`）、
**worker pid 未动**。

### 边界

- 该配置**只在新起的 worker 上生效**。当前那台 decode 节点的 argv 是启动时固定的，
  仍是 `/metrics` 404 + `指标不可用`，**所以 503 也不变**。
- 因此**顺序很重要**：必须在请求第二台机器之前把参数配好，否则新节点落地后还要再重载一次模型
  （NAS 载入 15–20 分钟），而那台机器本身可能要排很久才拿到。

---

## 3. 部署方法上的两处修正

**预检必须取真实退出码。** 上一版脚本写的是
`if /root/tai-talea -config … -check-config 2>&1 | tail -5; then` —— 检查的是 `tail` 的状态，
**永远为 0**，等于没有预检。本版改为捕获输出与 `$?`，预检失败**不动任何东西**。

**上传失败的真因是 Git Bash 的路径转换。** `jz.py put <iid> <local> /root/tai-talea.new` 里的
`/root/tai-talea.new` 被 MSYS 改写成 `D:/…/root/tai-talea.new`，于是 `sftp.stat` 报 `ENOENT`，
看起来像平台的 sftp 不接受新路径。加 `MSYS_NO_PATHCONV=1` 即解决。

顺带确认这个平台的长连接确实不可靠（单条 exec 通道传 19.5 MB 会 `Socket is closed`，
与「远端后台进程必须立即断开」同源），所以新增 `jz-upload-chunked.py`：
512 KB 一块、每块一条新连接、**逐块校验远程字节数**，19,568,104 字节用 1m46s（0.2 MiB/s）。

---

## 4. 当前状态与下一步

| | 状态 |
| --- | --- |
| 控制面 | v0.4，`0.0.0.0:8787`，`mode: fixed`，队列语义/换名修复已生效 |
| 容器 | `glm53-prefill`（H800A×8，角色 `decode`，已在服）+ **`glm53-pd2`（QUEUED，见 §5）** |
| 指标 | 当前节点不可用（`--enable-metrics` 未随启动下发）；**下一台起可用** |
| 弹性 | 仍未开启：`mode: fixed`，且单节点下 `Snapshot.Complete` 不可能为真 |

下一步按价值排序：

1. ~~请求第二台机器~~ —— **已完成**，见 §5。
2. 新节点落地后：`--enable-metrics` 生效 ⇒ 两侧样本可用 ⇒ `Snapshot.Complete` 可能首次为真
   ⇒ 才谈得上把 `mode` 改 `auto` 并观察第一次真实档位移动。
3. 若要让**当前这一台**不再 503：Router 去掉 `--pd-disaggregation` + SGLang 以聚合模式重启
   （模型重载 15–20 分钟）。注意这是临时形态 —— 第二台到位后 Router 要再切回 PD，成员表要重建。

---

## 5. 第二台机器：已发出（2026-10-13 10:33:28）

走的是控制面自己的入口，不是手工调平台 API —— 这样「console 与自动扩容走同一条创建路径」
这个不变量才有意义，也才顺带验证了 §3 之外的新语义。

```
POST http://127.0.0.1:8787/v1/capacity/console/provision
Authorization: Bearer <server.admin_token>
{"partner_id":"alayanew","name":"glm53-pd2","count":1}

→ 202 {"accepted":true,"jobs":[{"name":"glm53-pd2","partner_id":"alayanew","state":"QUEUED",…}]}
```

用**管家令牌**（`server.admin_token`）通过 `consoleAuth` 时 `consolePartner(r)` 为空，
所以 body 里**必须**显式带 `partner_id`（`handleProvision` 会以「缺少 partner_id」400 拒绝）。
payload 受 `decoder.DisallowUnknownFields()` 约束，`count` 默认 1、上限 4。

### 观察到的状态流转

| 时刻 | 作业（控制面） | 平台 |
| --- | --- | --- |
| 10:33:28 | `QUEUED` | — |
| 10:33:28.6 | `CREATING` / 等待平台受理 | create 已受理，`createTime 10:33:29` |
| 10:33:43 | `WAITING_SSH` / 等待容器 RUNNING 并获取连接信息 | **`QUEUED`** |

平台侧记录（`GET /api/osm/v1/cci/instance/list`）：

| name | id | status | priority | GPU | createTime | releaseTime |
| --- | --- | --- | --- | --- | --- | --- |
| **glm53-pd2** | `543635f8-432f-42fe-8a82-8d7bce82ca33` | **QUEUED** | **1** | 8×H800A | 2026-10-13 10:33:29 | `None` |
| glm53-prefill | `c8752ad0-2818-45e0-a7c5-52a2c7c13030` | RUNNING | 3 | 8×H800A | 2026-09-30 11:50:33 | `None` |

三个结论：

1. **名字是干净的** —— 没有回 `212035`，说明 `glm53-pd2` 从未被用过。
   （已释放的 `glm53-auto4` / `glm53-auto5` / `glm53-decode4` 不在 `instance/list` 里，
   但平台仍永久保留它们。）
2. **`priority: 1` 真的下发了** —— 平台记录 `priority=1`，对比老机器是平台默认的 3。
   注意这个字段是**实例属性**，只有创建时决定一次。
3. **平台没有释放它** —— `releaseTime` 为空，`status` 是 `QUEUED` 而不是 `RELEASED`。

### 这一版语义的判据（怎么算通过）

作业停在 `WAITING_SSH` 是**正确**行为，不是卡住：容器不存在 ⇒ `awaitingCapacity("QUEUED")` 为真
⇒ 适配器把 `deadline` 停在零值（预算停表）、按 15s 轮询，直到池子里腾出节点。
真正的失败点只剩外层 `provision_timeout`（6h）；届时返回 `ErrAwaitingCapacity`，
`runProvision` 把它 **hold 成 `QUEUED` 而不是 release**，队列位次保住。

对照旧行为：上一轮 `glm53-auto5` 在 `createTime 19:57:16 → releaseTime 01:27:33`，
**恰好 5h30m**（`wait_timeout`）—— 那是我们自己的超时把队列位次**主动丢弃**了。

观测设施：容器内 `/root/pd2-watch.sh`（pid 见 `pgrep -af '[p]d2-watch'`），
每 60s 把**去重后**的作业状态追加到 `/tmp/pd2-timeline.log`，可跨 SSH 会话保留时间线。

### 未定

- 池子何时腾出节点无法预测：平台没有任何容量/队列查询接口（`/cci/product/stock` 等全 404），
  排队只能靠 `priority` + 等。
- 若 6h 内没等到，作业会被 hold 成 `QUEUED` 且**仍计入 `OutstandingProvisions`**
  （planner 不会重发），此时需要人决定：继续等（再发一次）还是放弃（手工 release）。
