# AlayaNeW CCI 部署交接文档（2026-11-03）

> 本文档取代 `alayanew-handover-2026-11-02.md`（其 §4「STOP/START 清 overlay」的
> 记录已被实测推翻，见 §5.4）。11-02 版保留作历史流水；本版是当前事实的完整快照。
> 改进项台账（P1-1～P1-13）与验收记录（G0～G4）见
> `alayanew-improvement-plan-2026-11-02.md`，本文不重复。

---

## 0. 一句话总览

两台 CCI 容器（8×B300 each）跑 GLM-5.3-Flash 的 P/D 分离推理服务，控制面
tai-talea（Go）管容量与自愈，Router（sglang-router）管路由，nginx 做公网门面
（限流 + API key）。**控制面版本 0.9.10，全部 P1 整改关闭，G4（故障与恢复）
验收通过。自动扩缩容（G3）在本账号下永久搁置——见 §10。**

当前健康快照（2026-11-03 部署后）：双实例 SERVING、readyz 全绿、synthetic
端到端探针通过、e2e ~0.5s。

## 1. 拓扑与组件

```
公网客户端
   │  HTTP :8000 + Bearer <TALEA_API_KEY>
   ▼
[glm53-pd2b]  内网 172.19.213.160   ← 控制面节点兼 prefill worker
   ├─ nginx       :8000  门面：限流(20r/s burst40 / 并发64/IP)、key 校验、/health
   ├─ tai-talea   127.0.0.1:8787   控制面：容量、provision、告警、SQLite
   ├─ sgl-router  127.0.0.1:8001   P/D 路由（health 用 /server_info，勿改 /health）
   ├─ bootstrap   0.0.0.0:9101     本机 worker 的节点管理（token 鉴权）
   └─ sglang prefill 0.0.0.0:9102  （P1-8 齿轮 1:1）
[glm53-pd3b]  内网 172.19.166.155   ← 纯 decode worker
   ├─ bootstrap   0.0.0.0:9101
   └─ sglang decode  0.0.0.0:9102
```

- 模型：`zai-org/GLM-5.3-Flash`，FP8，62 shard / 306 GB，来自 NAS `/root/gongji`。
- Router 注册按 **URL** 去重；worker URL 是 `http://<内网IP>:9102`。
- ⛔ 容器的 **9001/9002 是平台强制发布的公网端口**，任何进程不得监听（两处
  脚本都有 preflight 拦截）。

## 2. 访问方式（凭据不入库，只写来源）

| 什么 | 从哪拿 |
| --- | --- |
| worker SSH | 平台 API `get-ssh`（本机工具 `.g4-power.py ssh <name>` 输出 url+password）；实例 **STOP/START 后端口密码不变**，实例**重建**后必变 |
| 控制面 admin token | pd2b `/etc/tai-talea/tai-talea.yaml` 的 `admin_token` |
| 门面 API key | pd2b `/root/.tai-talea-secrets` 的 `TALEA_API_KEY`（edge 渲染 nginx conf 用同一个） |
| bootstrap token | pd2b config `controller.bootstrap_token` ＝ 两台 worker `/etc/tai-talea/bootstrap-token`（必须一致） |
| 加密归档口令 | 运维私存（`TALEA_BACKUP_PASSPHRASE` 在 secrets 文件；另有独立归档口令由账号所有者保管，**丢了归档解不开**） |
| 平台 API AK/SK | 本地 `.remote-plane.yaml`（不在节点上） |

本机操作工具（D:/DagoPlus/.workbuddy/tmp/alayanew/）：`cci-ssh.py <host> <port>
<pwd> run|put|get`（**put 必须加 MSYS_NO_PATHCONV=1**）；`rs.py`/`sshslotd.py`
只通 n2 跳板，不通 CCI 实例——别再用错通道。

## 3. 日常运维

```bash
# 状态一屏
curl -s http://127.0.0.1:8787/readyz
curl -s -H "Authorization: Bearer $T" http://127.0.0.1:8787/v1/capacity/instances
curl -s http://127.0.0.1:8001/workers            # router 名册（8001 本机才可达）
curl -s http://127.0.0.1:8000/v1/models          # 无 key 应 401

# e2e（走门面，即 synthetic 探针的完整链路）
KEY=$(grep '^TALEA_API_KEY=' /root/.tai-talea-secrets | cut -d= -f2)
curl -s http://127.0.0.1:8000/v1/chat/completions -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}'

# 指标（探针计数、告警计数等）
curl -s http://127.0.0.1:8787/metrics | grep -E "synthetic|alerts"

# 日志（均在 /opt/tai-talea/logs/）
control-plane.log / router.log / bootstrap.log / sglang-{prefill,decode}.log
# ⚠️ grep 前先 sed 's/\x1b\[[0-9;]*m//g' 剥 ANSI，否则统计失真
```

重启组件：**优先让 watchdog 干**（30 秒内自拉），手工路径：
- 单杀控制面：`pkill -x tai-talea && setsid nohup /root/tai-talea -config /etc/tai-talea/tai-talea.yaml > /opt/tai-talea/logs/control-plane.log 2>&1 &`
- 全套带断言：`TALEA_ADMIN_TOKEN=<token> bash /root/cci-start-plane.sh`（注意：会重启 Router，双 worker 重注册 ~20s）
- edge：`set -a; . /root/.tai-talea-secrets; set +a; bash /root/cci-edge.sh up`
- worker bootstrap：`TALEA_BOOTSTRAP_TOKEN=$(cat /etc/tai-talea/bootstrap-token) bash /root/cci-start-worker.sh`（verify 现为轮询 30s）

## 4. 自愈与告警体系

- **watchdog**：`cron @reboot` 起一条 `/opt/tai-talea/sbin/cci-watchdog.sh loop 30`，
  每 30s 依次检查并拉起 **router → plane → edge → 本机 bootstrap**（worker 节点
  跳过前三者）。bootstrap token 从 `/etc/tai-talea/bootstrap-token` 读，不依赖 env。
  ⛔ **STOP/START 后 cron 不自启**——cci-start-worker.sh 开头会幂等拉回；这也是
  STOP/START 恢复序列必须先跑 worker 脚本的原因。
- **synthetic 探针**（0.9.10+）：控制面每 5 分钟经门面发一条真实 completion
  （`synthetic_url`+`synthetic_api_key` 已配）；非 200 或无 choices = 失败；连败
  2 次告警 `synthetic_probe_failed`(critical)。指标 `synthetic_probe_total`。
- **streak 机制**：unreachable 连败 3 轮才判 LOST（`lost_failure_threshold`）；
  health 连败 3 轮才判死（`health_failure_threshold`）；摘流（readiness）2 轮。
- **revoke 宽限**（0.9.9+）：快照缺席 10 分钟（`revocation_grace`）才判
  CAPACITY_REVOKED；重现即原谅重置。
- **start 退避**（bootstrap /3 生效后）：waitHealthy 前 5s 200ms、之后 2s；
  bootstrap 对轮询路径的 200 不记日志。

告警名清单：`synthetic_probe_failed`、`instance_lost_rate_high`、
`partner_sync_failed`、`reconcile_failed`、`load_unavailable`、
`service_start_failed`、`bootstrap_failure`、`drain_timeout`、
`readiness_drain_failed` 等（全量见 `internal/obs/alerts.go`）。
**目前只有 LogSink（日志）**——webhook 未配（§10）。

## 5. 故障手册

### 5.1 kill 单进程 → 自愈时长参考

nginx ~17s；控制面 ~12s；Router ~12s（双 worker 重注册）；SGLang ~5min（60s 置
STARTING 重派）；bootstrap 闭环但**代价 = 模型重载 5-20 分钟**。

### 5.2 503 的判别

池级背压（超 192 并发）= 429 + `Retry-After: 1`、耗时 0ms。**503
`no_available_workers` 不是熔断也不是排队**：整池加载中 / 未注册好 /
配置放行但队列门满。判据：360-500ms + 控制面日志无 token 字段。

### 5.3 LOST 但实例活着

八成是**内网 IP 变了**（见 5.4）或 endpoint 旧。修 endpoint 后 LOST→STARTING→
SERVING 自动闭环（~8 分钟）。日志先剥 ANSI 再 grep。

### 5.4 容器 STOP/START（完整恢复序列）★ 最常用

**事实**：overlay **保留**（/opt、/etc、/root 的部署物、DB、secrets、NAS 全在），
丢的只有**进程状态**与**内网 IP**；SSH 端口密码不变。平台 START 返回 RUNNING ≠
list 立即含实例（~3 分钟窗口，0.9.9 的 revoke 宽限已覆盖）。

恢复序列（在实例上）：
1. 推最新 `cci-start-worker.sh`（如仓库有更新）；
2. `export TALEA_BOOTSTRAP_TOKEN=$(cat /etc/tai-talea/bootstrap-token) && setsid nohup bash /root/cci-start-worker.sh`（后台，带回 cron，模型开始加载）；
3. `sed -i 's/<旧IP>/<新IP>/' /etc/tai-talea/tai-talea.yaml`（advertise_hosts，仅控制面节点）；
4. 控制面未起时改库：`sqlite3 /var/lib/tai-talea/tai-talea.db "UPDATE capacity_instances SET endpoint='http://<新IP>:9101', service_endpoint='http://<新IP>:9102' WHERE id='glm53-pd2b';"`（decode 行同样改）；
5. `TALEA_ADMIN_TOKEN=<token> bash /root/cci-start-plane.sh`；
6. `set -a; . /root/.tai-talea-secrets; set +a; bash /root/cci-edge.sh up`；
7. `rm -rf /opt/tai-talea/run/watchdog.lock && setsid nohup /opt/tai-talea/sbin/cci-watchdog.sh loop 30 >> /opt/tai-talea/logs/watchdog.log 2>&1 &`；
8. 等模型加载回 SERVING（NAS 冷读最长 ~30 分钟，62 shard × ~21s），e2e 验证。

worker 节点（pd3b）做 1-2-4(decode 行)-7 即可，无 3/5/6。

### 5.5 实例重建（release 后重新 create）——与 STOP/START 不同！

overlay 真没了：走 `cci-unpack-runtime.sh`（NAS runtime）→ worker 脚本 →
cci-config.sh → cci-start-plane.sh 全链路，见 `deploy/alayanew/` 的 README。

### 5.6 SQLite 恢复

每日 04:17 `sqlite3 .backup`（本地 7 天保留）。隔离恢复演练手册：`.g4-sqlite-restore.sh`
思路——独立端口 + 独立 DB 路径起临时控制面，看 recovery `checked=N`，live 库
md5 不变。旧 schema 备份会被自动迁移（当前 v5）。

### 5.7 日志/磁盘

logrotate（copytruncate+gzip）已管住 bootstrap.log / sglang-*.log / nginx 等；
磁盘水位告警有 `TALEA_DISK_FORCE_PCT=1` 强制路径可真机演练。

## 6. 备份与制品

- **运行时制品**：NAS `/root/gongji/tai-talea-runtime/`（venv tar 11.2G + bits + jit caches + SHA256SUMS）。
- **信任锚**：`tai-talea-manifest.sha256` 钉全部制品 hash，一份存 **NAS 之外**
  （pd2b `/etc/tai-talea/artifact-manifest.sha256`，本机另有一份 `.g4-manifest.sha256`）。
  NAS 与清单同时被篡改过不了校验（P1-13）。
- **私有归档**：`cci-pack-runtime.sh` 两信任域打包加密；`cci-restore-plane.sh`
  显式核验 manifest 后恢复。**归档口令丢失 = 归档作废**，口令在账号所有者手里。

## 7. 安全模型

- worker 的 `/etc/tai-talea` **只有** `bootstrap-token` + `profile.json`；
  控制面 yaml/DB/二进制/平台凭据一律不在 worker（G4-5 已复验，含全盘字符串扫描）。
- 门面限流：per-IP 20 r/s burst 40、并发 64/IP，**server 级生效——401 爆破也受限**；
  401/403/429 独立记 `edge-authfail.log`。
- key 轮换演练通过：改 secrets → `cci-edge.sh up`（nginx -t + 原子替换 + reload
  不中断），旧 key 立即 401。
- TLS：平台 `open-port` 的 https 只是标签（TCP 透传），**公网 TLS 需域名+真证书
  在 nginx 终止**；当前裸 IP + 明文 HTTP + key（§10 待决策）。

## 8. 配置参考（pd2b /etc/tai-talea/tai-talea.yaml）

易忘的键（完整 schema 见 `internal/config/config.go`，有长注释）：

| 键 | 现值 | 说明 |
| --- | --- | --- |
| `controller.revocation_grace` | 10m | 快照缺席多久才信是真 revoke（0.9.9） |
| `controller.synthetic_interval` | 5m | 端到端探针周期，0=关（0.9.10） |
| `controller.synthetic_url` / `synthetic_api_key` | 门面 / secrets | 探针打门面=全链路 |
| `controller.lost_failure_threshold` | 3 | unreachable 连败判 LOST |
| `controller.start_timeout` | 90m | 冷加载预算，勿调小 |
| `controller.drain_grace` | 300s | 释放前排水窗口 |
| `planner.mode` / `capacity_actions` | fixed / false | **G3 开关，见 §10** |
| `router.mode` | readiness-v1 | health 走 /server_info（/health 会误伤 decode） |

代码默认值兜底：config 缺键时用 `config.Default()` 的值（如 grace 缺省 10m）。

## 9. 版本与部署

- 版本轨迹：0.9.8（pull 轮次自报错）→ **0.9.9**（revoke 宽限，616dd76）→
  **0.9.10**（synthetic 探针，65710a3，现役）。旧二进制备份在 pd2b
  `/root/tai-talea.bak-0.9.8 / .bak-0.9.9`。
- 控制面部署：本地 `GOOS=linux GOARCH=amd64 go build -ldflags "-X …buildinfo.Version=… -X …Commit=…"` →
  scp 到 `/root/tai-talea-g4x` → 备份旧二进制 → 替换 → `cci-start-plane.sh`（自断言，失败即失败）。
- **bootstrap 版本契约**：`BOOTSTRAP_VERSION`（Python `__init__.py`）必须等于
  `main.go` 的 `bootstrapVersion` 常量；**`ErrBootstrapVersion` 是硬失败不自动重装**，
  必须双侧同发。仓库已 bump `/3`（P1-4 日志静默），**现役仍是 `/2` 对 `/2` 自洽**；
  `/3` 随下次全量部署（新 runtime 制品 + 实例重建/STOP 窗口）生效。
- 全量测试门禁：`go test ./...` + `go vet ./...` + `python -m unittest discover -s bootstrap/tests -t .`。

## 10. 已知限制、搁置项与待办

- **G3 自动扩缩容：本账号下永久搁置。** 账号 GPU 配额 16 卡，现役 2×8=16 已占满，
  第三台在数学上开不出来（弹性池取卡超时另有取证）。planner 维持 `fixed` +
  `capacity_actions: false`；代码链路已就绪（P1-9/P1-11），若未来换配额，先过
  G3 验收再开 auto。
- **webhook 告警外投：挂起**——`alerts.webhook_url` 发原生 JSON，钉钉/企微机器人
  需格式转换（加一个 sink 或中转），等账号所有者提供接收端。
- **公网 TLS：待决策**——需要域名 + 证书在 nginx 终止。
- **账号所有者待办**：① 吊销历史外发过的旧凭据；② 改平台控制台密码；③ 向平台
  核实 QUEUED 状态是否计费；④ 保管归档口令（已提醒，丢失无法补救）。
- 已知无害行为：watchdog loop 与 cron @reboot 偶发双触发（幂等）；
  `cci-start-plane.sh` 重启 Router 导致 ~20s 重注册窗口。

## 11. 文档索引

| 文档 | 内容 |
| --- | --- |
| `alayanew-improvement-plan-2026-11-02.md` | P1 台账（全关）+ G0-G4 验收记录 + 风险表 |
| `alayanew-handover-2026-11-02.md` | 上一版交接（历史流水；§4 overlay 说法已被本文修正） |
| `alayanew-cci.md`、`alayanew-1p1d-acceptance-2026-10-26.md` | CCI 接入细节、1P1D 验收 |
| `automation-review-2026-10-27`、`node-permission-inventory-2026-11-01` | 自动化缺口盘点、节点权限清单 |
| `architecture-and-design.md`、`provisioning-design.md`、`user-guide.md` | 设计与使用说明（跨环境） |
| `kunhaoyuan-handover-2026-11-01.md` | 中国移动 B300 环境（另一套现役） |

—— 交接完。改配置前先读 §8 的键注释；动 worker 前先想 §5.4 的 IP 事实；
碰凭据前先看 §2 和仓库红线（**仓库公开，凭据永不入库**）。
