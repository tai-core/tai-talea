# 三节点真实部署与验证记录

日期：2026-09-18，时区 Asia/Shanghai。证据保存在 `D:\DagoPlus\.workbuddy\tmp\three-node-20260918\evidence`。

## 结论和边界

已在用户提供的三个容器上跑通内部 SGLang fork 的 1P1D 推理：控制节点运行 Talea 和 Router，两个工作节点运行 Prefill/Decode。Talea 完成容量 ADD 后的角色分配、服务启动、健康确认和 Router 注册。真实聊天请求返回正确结果；Talea 和 Router 分别重启后，P/D 进程未重启，推理继续成功。

**尚未完成“提供裸容器后全程无需人工/AI agent”的验收。** 本轮依赖适配、制品下发、venv 安装和 bootstrap 首次启动使用人工编排的 SSH 联调脚本。首次铺装必须产品化为 Talea 的可恢复任务后重新验收；不能将本报告的成功范围扩大为整个项目完成。

## 部署基线

| 项目 | 本轮实际值 |
|---|---|
| control | 172.19.116.74，Talea API 127.0.0.1:8787，Router 推理 30000、管理 127.0.0.1:30001 |
| talea0 | 172.19.116.115，Prefill，bootstrap 9001、推理 9002、P 握手 8998 |
| talea1 | 172.19.111.251，Decode，bootstrap 9001、推理 9002 |
| 平台环境 | Ubuntu 22.04.5、系统 Python 3.10.12、CUDA toolkit 12.8、驱动 580.173.02；容器内无 systemd |
| GPU | control H100 80GB ×1；两个 worker 各 H100 80GB ×4，本轮各使用 GPU0、TP=1 |
| 模型 | `/root/public/models/Qwen/Qwen2.5-3B-Instruct`，使用平台已存在的模型文件 |
| Talea / bootstrap | 远端运行 `54dcc9c` 基线；本轮本地修复尚未部署、未提交 |
| Router | `70839ad`，Rust 1.93.1 release 构建，程序版本 0.3.2 |
| SGLang | 私有 fork `5a26fc1f46a88be023d23c3f8fc896a8555a29e7`，wheel `0.0.0+g5a26fc1f.cu12` |
| 依赖 | Torch 2.11.0+cu128、torchvision 0.26.0+cu128、sglang-kernel 0.4.3+cu129、cuda-python 12.9.4、triton 3.6.0、NIXL 1.4.1 |
| 制品 | 196 个 wheel，合计连同 requirements 5,648,378,409 字节；逐文件 SHA-256 见 runtime-manifest.json |
| P/D 传输 | 原生 NIXL，UCX backend，eth0；UCX_TLS=tcp,cuda_copy,cuda_ipc,sm,self；未暴露 /dev/infiniband |

fork 的打包依赖将 `cuda-python==13.2.0` 改为 `12.9.4`；本轮未修改其推理逻辑。不能把结果描述为原始依赖组合未经修改即通过。初始 Torch 2.9.1、2.10 与该 kernel 的 ABI 不兼容，最终 2.11 组合完成 GPU 内核导入与推理。

Router 二进制 SHA-256：`0d09e53bb12f45584bab049f442bae8bc2f441d4c06187bef3a65ff8361bf4db`。依赖锁与制品清单已保存在上述 evidence 目录；私有源码和 wheel 放在节点 `/opt/tai-talea`，未放入平台公共模型目录。

## 已执行的真实场景

| 场景 | 结果 | 证据 |
|---|---|---|
| 两个 worker 环境装配 | pip check、兼容 profile、torch/sglang/sgl_kernel 导入、NIXL/UCX 初始化通过 | talea0-install-worker.log、talea1-install-worker.log、runtime-requirements.txt |
| Push ADD | 两个实例分别接收成功，创建容量记录；紧接重复事件返回 duplicate=true | add-1789719839847162913.json、add-1789719839894188280.json |
| Talea 自动启动与注册 | 最终两实例 SERVING，Router 两个健康 worker、角色分别 prefill/decode，readiness ready | recovery-1789720500720928439.json 的 before |
| 真实 P/D 请求 | 16:31:12，询问“2+3，仅回答数字”，返回“5”；43 prompt tokens、2 completion tokens | infer-1789720272857236386.json；两个 worker 的 sglang 日志 |
| Talea 进程重启 | 约 3.01 秒后观察到健康状态；P/D PID、started_at、role、history 不变，重启后推理成功 | recovery JSON 的 talea 阶段、infer-1789720468321508994.json |
| Router 进程重启 | 约 32.20 秒后 Talea 自动恢复两池 membership/readiness；P/D 进程身份不变，推理成功 | recovery JSON 的 router 阶段、infer-1789720500714487360.json |

P/D PID 分别为 talea0 `4215`、talea1 `3600`。重启恢复时间是该次实验观测值，不是 SLA；重启测试没有持续并发流量，不证明零丢请求。首个请求耗时约 0.138 秒，后两次约 0.031 秒；样本不足，不作为性能指标。

本轮临时制品服务仍在 control 内网 9080，以文件 token 鉴权，仅用于后续铺装联调。测试服务继续保留。节点重启自动拉起尚未实现，不应把进程 restart 测试等同于容器或宿主机 reboot 验收。

## 本轮发现与本地修复

1. 使用 venv Python 启动 SGLang 时，子进程 PATH 仍来自系统环境，JIT 找不到 venv 中的 ninja。远端通过启动脚本加入 venv/bin 后继续测试；正式修复已写入 `bootstrap/tai_talea_bootstrap/service.py`，统一设置子进程 VIRTUAL_ENV/PATH。
2. bootstrap 对启动超过 0.25 秒后的子进程退出未更新状态。本地补丁让 status/health/start 刷新退出事实、记录退出码与时间，允许后续重试。
3. 控制面健康等待遇到 bootstrap FAILED 时仍等待整个 start_timeout。本地补丁在明确进程失败时立即结束等待并走已有失败记录路径。

本地验证：`python -m unittest discover -s bootstrap/tests -v`，55 条通过；`go test ./...` 和 `go vet ./...` 通过；`git diff --check` 通过。新增测试覆盖延迟退出、重试、真实子进程环境，以及失败原因持久化且不注册 Router。这些补丁尚未替换远端运行版本。

另外，冷启动真实暴露 Router 异步注册尚未可见时 readiness 返回 404 的暂态问题，引发自动重试及一次多余 Prefill 摘流；最终虽恢复，仍需修复。详细风险和实施安排见 `review-and-next-plan-2026-09-18.md`。

## 未完成的验收

- 裸容器首次铺装、铺装中控制面重启续作、取消和租约代际隔离。
- 在途/流式请求的安全摘流；未知负载、Stop 失败、撤销后不重新启用等故障分支。
- 杀掉 SGLang、重启 bootstrap、节点失联恢复、同地址角色切换。
- 真正的合作商 Pull 与云端释放接口；当前使用 static adapter，不能据此声称云容器已可自动销毁。
- 多卡、并发、长时间稳定性、RDMA 性能和多 Router 高可用。

下一步以持久化操作、撤销优先级、首次自动铺装和异步注册收敛为主要交付，不依赖运行期人工排错脚本来补足产品能力。

## 后续增量：控制台与回收入口（同日 17:08）

控制节点已升级为 `dev-console-20260918 (54dcc9c-dirty)`，二进制 SHA-256 为 `88815b7131049d99d0763531f29bb0a0e92702811b04337a92125a80e03cb300`；保留旧二进制 `/opt/tai-talea/bin/tai-talea.before-console`。本地源码尚未提交，不能把此版本等同于仓库 HEAD。

新增根路径管理页面、管理员 `POST /v1/capacity/instances/{id}/reclaim` 和 `/usr/local/bin/talea` 命令。回收意图先原子写入 SQLite，后台推进并持久化重试；增加调度排除、启动中回收协调及未知负载等待。页面通过独立测试数据验证交互；Go 集成测试验证真实 HTTP API、SQLite 与控制器，远端确认新路由已加载并正确拒绝不存在的实例。

升级前后 talea0/talea1 的 SGLang PID 仍分别为 4215/3600，两个实例仍 SERVING；再次真实推理返回“5”。未通过按钮回收真实的 talea0，留给用户操作。升级证据为 evidence/console-before.txt、console-after.txt，详细用法见 `console-and-client.md`。

上述旧报告中“远端 Talea 仍为基线”的描述只适用于 16:35 前的测试；本次 Go 控制面已经包含本地补丁。两个远端 bootstrap 尚未升级，首次自动铺装、真实 GPU 在途摘流和其他未完成场景仍保持未验收。
