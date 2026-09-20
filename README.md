# tai-talea

管理合作商提供的 GPU 容器，在节点内启动 SGLang Prefill/Decode 服务，并通过 sglang-router 动态接流、摘流与回收。

日常操作由控制节点上的 Talea 自动执行，不需要 AI agent。合作商负责提供容器；Talea 接管已有容量，不向平台申请创建新容器。

## 文档入口

- [使用说明书](docs/user-guide.md)：访问页面、上线、回收、实时监控、压测和常见问题。
- [项目说明书](docs/project-guide.md)：架构、构建部署、配置、接口、数据备份与当前边界。
- [本轮审查与验证](docs/review-2026-09-20.md)：缺陷修复、验证结果和未验收场景。
- [SSH 安装配置](docs/ssh-onboarding.md)、[监控指标与 benchmark](docs/pd-monitoring-and-benchmark.md)：需要具体参数时查阅。

## 当前能力

| 功能 | 状态 |
|---|---|
| 网页 SSH 上线、任务进度、同名节点新租约 | 已实现 |
| 一键回收、readiness 摘流、停止与释放重试 | 已实现 |
| Push 认证、签名、幂等、持久化事件恢复 | 已实现 |
| 单 Router、SQLite、周期健康检查与状态恢复 | 已实现 |
| 固定 P:D 档位、冷却和每轮变更限制 | 已实现 |
| P/D 吞吐、队列、KV 指标、并发压力曲线 | 已实现 |
| 控制节点 GSM8K 压测 | 已实现 |
| Pull 完整快照与实际容量对账 | 已实现，默认关闭；需适配平台契约并验收 |
| 实时自适应 P/D、多控制面高可用、多 Router | 未实现 |

容器状态与服务状态分开保存。历史字段 `IDLE` 表示容器已就绪，`IDLE + SERVING` 在页面和客户端显示“服务中”；只有没有服务且没有回收请求才显示“空闲”。

## 开发验证

需要 Go 1.25+、Python 3.10+；SSH 安装器额外需要 `provision/requirements.txt` 中的依赖。以下在 Linux/WSL 或兼容 shell 执行：

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r provision/requirements.txt
make test
make vet
make build
./bin/tai-talea --config deploy/tai-talea.example.yaml --check-config
```

构建输出 `bin/tai-talea`、`bin/talea`、`bin/talea_benchmark.py`。示例配置用于展示字段，必须替换凭据、运行时版本、模型和节点地址；仅启动 Talea 不会自动部署 Router 或生成 wheelhouse。

受支持的工作节点通过“上线”安装固定运行时：SSH 探测 → SFTP 分发校验制品 → 本地创建 venv / 离线 pip 安装 → bootstrap → Planner → SGLang → Router。Python 依赖和模型由控制节点分发，系统依赖仍需要 apt 软件源。

当前验证基线是 Ubuntu 22.04 / Python 3.10 / CUDA 12.8 / H100，内部 SGLang fork `5a26fc1f`、NIXL/UCX TCP、Qwen2.5-3B、TP1/DP1。全新无缓存镜像、真实云平台容器销毁和开机自启尚未完整验收。正常摘流等待在途请求，超过宽限期可能强制停止，不能保证故障或超时下请求零损失。
