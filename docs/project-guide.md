# Talea 项目说明书

## 1. 目标与架构

Talea 是独立的 SGLang 容量控制面，纳管合作商已经提供的容器，完成 P/D 角色分配、启动、注册、摘流与回收。业务请求走 Router，不经过 Talea。日常流程是固定程序自动执行，不依赖 AI agent。

```text
合作商 Push / Pull / 网页 SSH 上线
                  ↓
控制节点：Talea → SQLite 意图/事件/操作/审计
          ├─ SSH 安装器 → 工作节点运行环境
          ├─ Planner → bootstrap → SGLang P / D
          ├─ RouterAdapter → sglang-router 管理接口
          └─ 指标采集 → 内置管理页面

推理客户端 → sglang-router → Prefill ⇄ Decode
```

Router 可以与 Talea 共用控制节点，不要求独占 GPU 服务器。生产部署需要按吞吐和故障隔离决定机器配置。

| 目录 | 职责 |
|---|---|
| cmd/tai-talea、internal/config | 程序入口、配置加载与校验 |
| internal/controller、domain、store | 双层状态、事件恢复、生命周期和 SQLite 事务 |
| internal/partner | static / HTTP 容量适配器、Pull 快照 |
| internal/planner | 固定 1:1、2:1、1:2 档位及变更限制 |
| internal/routeradapter、launcher | Router 和 bootstrap 客户端 |
| internal/onboarding、provision | 加密任务队列、SSH/SFTP 安装与自检 |
| bootstrap | 容器内受控进程管理，不承担调度 |
| internal/telemetry、obs、api/ui | 实时负载、指标告警和内置前端 |
| tools、deploy | 管理/Push/benchmark 客户端、部署模板 |

## 2. 构建与测试

构建要求 Go 1.25+、Python 3.10+。bootstrap、客户端和 benchmark 使用 Python 标准库；SSH 安装器及其测试额外依赖 Paramiko。以下命令用于 Linux/WSL 或兼容 shell：

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r provision/requirements.txt
make test        # Go + bootstrap + CLI/benchmark + provision
make vet
make build
# 交叉编译 Linux 控制面：
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/tai-talea ./cmd/tai-talea
```

前端是嵌入 Go 二进制的 HTML/CSS/JavaScript，不需要 Node 服务。修改前端后必须重编译并重启 Talea。Node 仅可用于开发时的脚本语法检查。

## 3. 首次部署控制节点

1. 安装本仓库构建的 tai-talea，以及同目录下的 talea、talea_benchmark.py；为 CLI 建立 PATH 链接。
2. 独立部署兼容的 sglang-router，启用 PD、以空 worker 池启动；配置实际管理接口地址。已验证组合依赖私有 Router 的 readiness 实现，不能随意替换成任意公共版本。
3. 从 deploy/tai-talea.example.yaml 复制配置，替换凭据、模型、镜像版本、Router 地址、合作商和数据路径。P/D 的 `router.prefill_bootstrap_port` 必须对应运行时配置，当前测试用 8998。
4. 准备锁定版本的 wheelhouse、哈希清单、模型和 bootstrap 源，部署 provision，在独立 venv 安装 provision/requirements.txt，按 [SSH 安装配置](ssh-onboarding.md) 设置 onboarding。
5. 校验并启动：

```bash
tai-talea --config /etc/tai-talea/tai-talea.yaml --check-config
tai-talea --config /etc/tai-talea/tai-talea.yaml
```

6. 创建 `/etc/tai-talea/client.json`（0600）：

```json
{
  "url": "http://127.0.0.1:8787",
  "admin_token": "替换为与服务端一致的令牌",
  "router_url": "http://控制节点内网地址:30000",
  "model": "与控制器model_id一致"
}
```

`--check-config` 检查配置结构和基本约束，不代替 Router 连通性或 GPU 自检。示例配置中的旧版 CUDA/Python/SGLang 是字段示例，必须改成所用制品的实际版本。

当前三节点容器没有通用开机自启配置；进程监督由部署方结合平台启动脚本配置。有 systemd 的服务器可参考 deploy 下的 service 模板，并核对二进制路径、配置路径、服务账号对模型/制品的读取权限及任务目录写权限。

## 4. 工作节点安装与网络

网页 SSH 上线由控制节点串行处理：检查 root、系统/Python/GPU及主机密钥 → SFTP 分发 wheel/model/bootstrap 并核对 SHA256 → apt 系统依赖 → 本地 venv 与离线 pip → CUDA/内核/NIXL 自检 → bootstrap → 纳管。匹配哈希的缓存会复用。不会复制其他机器的 venv，也不会分发私有 Git 凭据。

当前验证组合：Ubuntu 22.04、Python 3.10、CUDA 12.8、H100；SGLang fork 5a26fc1f 打包版本 `0.0.0+g5a26fc1f.cu12`，Torch `2.11.0+cu128`、sgl_kernel `0.4.3+cu129`、NIXL 1.4.1，另含源码限定的 Decode 兼容补丁。模型为 Qwen2.5-3B-Instruct，服务 TP1/DP1，使用 GPU0。

| 网络入口 | 当前测试端口 | 要求 |
|---|---|---|
| Talea 页面/API | 8787 | 默认回环；通过 SSH 转发或认证 HTTPS 入口访问 |
| Router 管理 | 30001 | 限制为控制面可达 |
| Router 推理 | 30000 | 供推理客户端访问 |
| SSH / bootstrap | 平台映射 / 9001 | 控制节点可达；bootstrap 使用共享令牌 |
| SGLang / Prefill bootstrap | 9002 / 8998 | Router 和对端工作节点可达 |
| NIXL/UCX 数据连接 | 运行时动态端口 | P/D 内网互通，不能只开放上述固定端口 |

## 5. 接口、状态与恢复约定

| 入口 | 用途 |
|---|---|
| POST /v1/capacity/events | 合作商 Token + HMAC + 时间窗的 ADD/UPDATE/REVOKE |
| /v1/capacity/console/* | 管理员或合作商页面令牌，按归属隔离 |
| POST /v1/capacity/instances/{id}/reclaim | 保存完整回收意图，推荐的运维回收入口 |
| GET /v1/capacity/instances、GET /v1/capacity/instances/{id}/audit | 节点状态、审计 |
| GET/PUT /v1/capacity/planner | 查看或切换固定档位；API 修改只影响本次进程，持久化需改配置 |
| GET /healthz、/readyz、/metrics | 存活、启动就绪、Prometheus 指标；不代表整个 P/D 集群可推理 |

SQLite 保存意图，实际状态必须重新探测。PENDING 事件按有限批次轮转恢复；相同事件 ID 不能更换合作商或业务内容。旧版本已标记 ABANDONED 的历史事件不会自动复活，需要新事件或权威 Pull 重新确认。回收意图优先于启动和调度。运行中地址/租约更新先持久化目标，仍用旧身份摘流，停止后再切换。正常摘流只修改 readiness；异常旧角色清理必须先关闭流量并确认负载为零。

Pull 默认关闭。通用 HTTP 适配器要求两个接口都返回完整、无分页的 `{"instances":[...]}`，可显式带 `"complete":true`；拒绝字段缺失、null、不完整、分页字段和尾随数据。在租集合包含运行中的节点；available 必须是其子集。释放接口 404 按已不存在处理，409 按失败重试。平台字段/分页不同应编写对应适配器。每轮完整快照与当前容量意图对比，纠正 Push 导致的偏移；拉取期间的新事件以观测时间隔离，正在回收的容量不会被重新启用，同一已释放租约也不会复活。已通过协议模拟测试，真实平台仍需完成接口与并发时序验收；平台两份查询应提供一致的完整视图。

## 6. 数据与维护

- 配置：/etc/tai-talea/；SQLite：/var/lib/tai-talea/tai-talea.db（实际以配置为准）。
- 上线任务：onboarding.state_dir，包含 AES-GCM 凭据密钥、任务和已知 SSH 主机。终态清凭据，但目录仍需限制为服务账号可读写。
- 工作节点日志：/opt/tai-talea/logs/；benchmark：/opt/tai-talea/benchmarks/。
- 备份：使用 SQLite 在线 backup API，或停止 Talea 后完整备份数据库；不要只复制运行中的 .db 而漏掉 WAL。任务目录和 credentials.key 必须一起备份，配置/令牌单独保护。
- 更新：备份二进制、配置、数据库和任务目录；先校验配置和测试，再替换控制面。bootstrap 更新需走维护窗口，不能直接重启仍监督模型的进程。数据库迁移版本变更后，回滚需使用兼容二进制或对应备份。

## 7. 当前边界

已在受控 H100 环境完成真实 1P1D、并发压测、SSH 再上线及控制面/Router 重启验证；具体轮次见测试记录。未验证任意镜像适配、无缓存裸环境全流程、多卡 TP/DP、RDMA、大规模集群、真实平台销毁和整机 reboot 恢复。

尚未提供多 Router、高可用控制面、实时自适应 P/D、签名升级包和灰度回滚。Router 异常 PD draining 状态的完全自动恢复仍有边界。模型/Router 自身故障、网络永久中断和重试耗尽会暴露错误，需要运维处理；这不等于需要 AI agent 参与产品运行。
