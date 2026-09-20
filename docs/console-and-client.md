# 节点管理页面与简易客户端

Talea 内置管理页面，无需另起前端服务或安装 Node.js。启动控制面后访问其根路径，例如 `http://127.0.0.1:8787/`，使用管理员令牌登录。

页面显示纳管节点、服务中的 Prefill/Decode、回收处理中数量，支持搜索、节点详情与操作记录。点击节点旁的“回收”，核对节点和最长等待时间，再确认即可。回收请求写入数据库后立即返回，Talea 的后台协调器负责关闭 readiness、等待在途请求、停止服务、释放容量；页面每三秒更新。关闭浏览器不影响任务，重复请求不会重置宽限期。

## SSH 上线

右上角点击“上线”，填写节点名称、所属合作商、SSH 地址、端口、账号，以及密码或无口令私钥，然后点击“上线并交给 Talea”。内网地址默认自动探测，也可在高级设置中填写。已回收的节点支持“重新上线”，同名节点建立新租约并保留历史审计；正在服务的节点不允许覆盖安装。

Talea 控制进程的后台任务负责 SSH 连接、核对系统与 GPU、按 SHA256 校验并分发运行时轮子、同步模型、安装依赖、启动 bootstrap。安装完成后进入现有 Prepare → Planner → Start → Router 流程，角色由调度策略决定。若达到 `planner.max_serving`，接管后的新增节点可能保持空闲。页面“上线任务”显示阶段及失败原因，失败后点击“修改并重试”重新填写凭据。

任务以原子文件保存到 `onboarding.state_dir`，Talea 重启后恢复未结束的任务；安装结果先持久化，再提交数据库纳管，避免重启重复安装。SSH 凭据使用 AES-GCM 加密，密钥和任务目录仅供控制节点服务账号访问，成功或终态失败后清除任务中的凭据。备份时必须同时保留任务目录和其中的 `credentials.key`。首次 SSH 连接固定主机密钥，可选显式填写 SHA256 指纹；密钥变化时拒绝连接。

当前安装器支持本次验证的 Ubuntu 22.04 / Python 3.10 / CUDA 12.8 / H100 运行时，需要容器 root 权限、系统软件源可达，以及控制节点与 P/D 节点间的内网连通。bootstrap 使用 9001，推理使用 9002，P/D bootstrap 使用当前配置的 8998；仅有公网 SSH 端口映射而无容器内网互通时，还需要平台提供数据网络。不同系统或 Python 版本需管理员准备对应运行时制品。本轮在已回收且缓存仍在的 talea0 上验证了完整接管与重新安装检查，不等同于对所有全新基础镜像完成验证。

合作商使用独立 `partners[].console_token` 登录，只能查看自己名下的节点、审计和上线任务，以及提交上线或回收。Push 的 token/HMAC 协议仍独立。当前测试合作商的页面令牌可由控制节点管理员执行以下命令获取并分配，令牌不要贴入日志或工单：

```bash
talea --config /etc/tai-talea/partner-console.json login-token
```

安装配置与制品格式见 [SSH 自动接管部署说明](ssh-onboarding.md)。

只有 `instance_state=RELEASED`、`service_state=NONE` 才表示完成。未知负载不会在宽限期结束前当作零处理；到期可以强停并记录原因。停止失败保持回收状态并重试，不再由 Planner 启动服务。请求在服务启动中到达时，先等该次启动调用退出，再停止，避免释放后迟到的启动留下孤立进程。

## 当前三节点环境

控制节点已监听 `127.0.0.1:8787`。在自己的电脑建立 SSH 转发（保持终端打开）：

```bash
ssh -N -L 18887:127.0.0.1:8787 -p 30041 root@120.220.102.21
```

浏览器打开 `http://127.0.0.1:18887/`。在控制节点执行 `talea login-token` 获取登录令牌；不要把令牌贴到工单或发给合作商。

当前 `jiuzhang-test` 使用 static adapter，回收会停止 SGLang、标记容量已释放，九章平台容器仍保留。HTTP partner adapter 配置释放接口后，Talea 才会通知平台释放。页面确认框会显示当前回收范围，并在回收最后一个 P 或 D 时提示对推理可用性的影响。

## 控制节点的命令

```bash
talea nodes
talea nodes --raw
talea status talea0
talea reclaim talea0 --grace 60
talea benchmark --requests 64 --concurrency 8 --max-tokens 256
```

`talea nodes` 默认显示节点的实际运行状态，如“服务中”“空闲”“摘流中”“已回收”，与页面列表和详情一致。`--raw` 额外显示底层容器生命周期与服务状态，`talea status` 仍返回完整 JSON 供诊断。

看板的 P/D 实时区域每 3 秒刷新输入/输出 token 速率、运行/等待请求、KV 在途、并发压力与近五分钟曲线。P、D 使用两张独立卡片，曲线支持悬停读数和左右方向键查看采样，不显示 P−D 压力差。数据由控制节点 Talea 后台采集。`talea benchmark` 应在控制节点运行，同目录需安装 `tools/talea_benchmark.py`；完整参数、数据集来源及统计口径见 [P/D 监控与压测](pd-monitoring-and-benchmark.md)。

底层 `instance_state=IDLE` 是历史命名，实际含义为“容器已就绪”，SGLang 是否运行由 `service_state` 表示。已分配角色的 `IDLE + SERVING` 显示“服务中”；只有 `IDLE + NONE` 且无回收请求才显示“空闲”。页面详情将原始 `IDLE` 标注为“已就绪”，避免把正在服务的节点误判为空闲容量。

上线任务按合作商、节点名和该次上线的租约标识关联当前状态。同名节点重新上线后，旧成功任务显示“已下线”，并保留提交时间；无法核实关联的旧任务显示“已完成”，不会借用同名新节点的“服务中”。失败任务保留错误记录；只有节点未在役、且没有同名正在执行的上线任务时，才显示紧凑的“修改并重试”入口。

客户端为 `tools/talea.py`，仅依赖 Python 3 标准库。安装到 PATH 后命名为 `talea`。默认读 `/etc/tai-talea/client.json`，也可用 `--config 路径` 或 `TALEA_CLIENT_CONFIG` 指定。管理员配置示例：

```json
{
  "url": "http://127.0.0.1:8787",
  "admin_token": "REPLACE_WITH_ADMIN_TOKEN"
}
```

配置文件权限设为 0600，避免其他用户读取。此客户端文件独立于服务端 YAML；客户端无需持有 bootstrap、Router 或平台登录凭据。

## 合作商 Push

合作商只保存分配给自己的 Push 凭据，不获得管理员令牌。配置示例：

```json
{
  "url": "https://talea.example.com",
  "partner_id": "partner-a",
  "push_token": "REPLACE_WITH_PARTNER_TOKEN",
  "push_secret": "REPLACE_WITH_PARTNER_SIGNING_SECRET"
}
```

```bash
talea --config partner.json push revoke talea0 --grace 60
talea --config partner.json push add --file node.json
```

`node.json` 为容量对象，例如：

```json
{
  "id": "talea2",
  "endpoint": "http://10.0.0.12:9001",
  "service_endpoint": "http://10.0.0.12:9002",
  "lease_id": "lease-20260918-2",
  "spec": {"gpu": "H100", "gpu_count": 4}
}
```

客户端自动补事件 ID、发生时间、认证和 HMAC 签名，仍使用原有 Push 协议。调用方若需重试同一事件，应保存客户端输出的 event_id 并传入 `--event-id 原ID`。业务字段与签名过程不再需要合作商手写脚本。

## 管理 API

新增 `POST /v1/capacity/instances/{id}/reclaim`，管理员 Bearer 认证，请求体可省略，或传 `{"grace_seconds":60}`；允许 0–86400 秒。省略时采用服务端 `controller.drain_grace`。HTTP 202 表示意图已持久化，不代表释放已经完成。状态和审计沿用现有 GET 接口。

`/drain` 保留角色调整用途，`/release` 保留无运行服务时的底层释放用途。管理页面和 `talea reclaim` 都使用新的完整回收入口。

## 验证范围

Go 集成测试覆盖 API 鉴权、合作商隔离、重复请求、加密凭据与清理、上线任务重启恢复、同名节点新租约、回收重启恢复、停止/关闭 readiness 失败、未知负载与启动中回收；Python 测试验证签名、制品篡改拒绝，以及运行中的容器拒绝安装。真实 SSH 上线已由 Talea 在 talea0 上执行，随后 1P1D 推理返回正确结果。其他镜像、跨网络与真实平台容器销毁仍需后续验收。
