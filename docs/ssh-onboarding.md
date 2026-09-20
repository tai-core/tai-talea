# SSH 自动接管部署

控制节点的 Go 进程拥有持久化任务队列，调用仓库内固定安装程序。业务运行不依赖 AI agent，也不接受网页传入的 shell、安装路径或包源。安装器依赖 Paramiko，目标容器只需 root SSH、Python 3、受支持的系统和 NVIDIA GPU。

## 控制节点配置

将 `provision/` 和 `bootstrap/` 部署到控制节点。独立 Python 环境安装 `provision/requirements.txt`，避免改变控制节点的推理依赖。服务端配置增加：

```yaml
onboarding:
  python: /opt/tai-talea/provision-venv/bin/python
  script: /opt/tai-talea/provision/ssh_install.py
  manifest: /etc/tai-talea/onboarding-manifest.json
  state_dir: /var/lib/tai-talea/onboarding
```

每个合作商可配置独立 `console_token`，至少 16 字符，与管理员、Push 和其他页面令牌不同；未配置则只有管理员能替该合作商操作页面。页面元数据只返回可选合作商 ID，不返回凭据。

## 运行时清单

清单由管理员生成和固定，不能由合作商表单覆盖。例如：

```json
{
  "profile": {
    "os": "ubuntu-22.04",
    "cuda": "12.8",
    "python": "3.10",
    "sglang": "0.0.0+g5a26fc1f.cu12",
    "wheelhouse": "/opt/tai-talea/wheelhouse",
    "virtualenv": "/opt/tai-talea/venv",
    "bootstrap_version": "tai-talea-bootstrap/1",
    "model_id": "/root/public/models/Qwen/Qwen2.5-3B-Instruct"
  },
  "wheelhouse_source": "/opt/tai-talea/wheelhouse",
  "wheel_manifest_sha256": "REPLACE_WITH_MANIFEST_SHA256",
  "requirements_sha256": "REPLACE_WITH_REQUIREMENTS_SHA256",
  "bootstrap_source": "/opt/tai-talea/onboarding-source/bootstrap",
  "model_source": "/root/public/models/Qwen/Qwen2.5-3B-Instruct",
  "model_target": "/root/public/models/Qwen/Qwen2.5-3B-Instruct",
  "probe_ip": "172.19.116.74",
  "environment": {
    "UCX_TLS": "tcp,cuda_copy,cuda_ipc,sm,self",
    "CUDA_VISIBLE_DEVICES": "0",
    "HF_HUB_OFFLINE": "1",
    "TRANSFORMERS_OFFLINE": "1",
    "SGLANG_DISAGGREGATION_NIXL_BACKEND": "UCX",
    "OMP_NUM_THREADS": "8"
  }
}
```

`wheelhouse_source/manifest.json` 包含 `files: [{name,size,sha256}, ...]`，轮子必须与锁文件匹配。安装器核对控制端文件哈希后，经 SSH/SFTP 分发至节点，再核对目标端哈希；已缓存且一致的文件直接复用。pip 使用 `--no-index` 从已验证 wheelhouse 安装。模型与 bootstrap 从管理员配置的本地源分发，也做传输哈希验证。当前实现逐个执行任务，避免同时铺装挤占带宽。

模型目录需要包含真实权重，`model_target` 与控制器 `model_path` 保持一致。当前固定运行环境目录为 `/opt/tai-talea/{venv,wheelhouse}`。容器系统依赖通过 apt 安装，因此首次系统依赖安装仍需要其 Ubuntu 包源可用。

## 接口与恢复

- `GET /v1/capacity/console`：当前登录身份、允许的合作商、上线功能状态。
- `POST /v1/capacity/console/onboarding`：提交 `request_id,id,partner_id,host,port,user,password`，密码也可替换为 `private_key`。高级字段为 `advertise_host,host_key`。
- `GET /v1/capacity/console/onboarding`：当前身份可见的任务，不包含 SSH 凭据。
- `/v1/capacity/console/instances` 及单节点详情、审计、`/reclaim`：按登录合作商隔离。原管理员接口保持管理员认证。

202 表示任务已保存，不代表已经服务。同一请求 ID 重试返回同一个任务；跨合作商或变更连接目标后复用请求 ID 会被拒绝。失败任务保留原因但删除凭据，“修改并重试”产生新的请求 ID。安装器拒绝覆盖 INSTALLING/RUNNING/STARTING/STOPPING 服务；完成后由控制器核对实例归属、活动地址及待切换地址冲突、旧租约是否 RELEASED，再开始新租约，旧生命周期任务同时终止。

单次安装超时会进入失败终态并清除凭据，需要修正后重试。控制面正常退出导致的任务取消则保留恢复信息，重启继续处理。bootstrap 停服以整个受控进程组退出为准；清理失败不会报告 STOPPED。

安装日志在目标容器 `/opt/tai-talea/logs/onboarding.log`，管理进程日志在 `bootstrap.log`。Talea 进程重启可恢复任务；这里没有配置容器/控制节点操作系统重启后的开机自启，需要结合平台启动脚本或已有服务管理器部署。

## 本次验收

2026-09-18，通过合作商 API 提交 talea0，Talea 自动完成安装与纳管，产生新租约 `ssh-acceptance-86f8a633479c4d029c20fcb92f6cfdc6`。Prefill PID 16068，原 Decode PID 3600 保持运行。首次推理探测在 Router 收敛期间返回 503，Talea 周期协调后恢复，随后相同请求返回 `5`。任务记录中的 SSH 密文在安装完成后清除。该节点已有运行时与模型缓存，本轮验证了复用与安装检查路径；全新基础镜像和网络不互通的实例还需专项验收。
