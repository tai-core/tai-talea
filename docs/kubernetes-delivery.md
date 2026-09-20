# Kubernetes 交付说明

这份交付物把工作节点做成可替换的容器镜像和 Helm Chart。镜像只负责运行 Talea bootstrap；控制节点仍是唯一的调度者，负责把节点纳入容量、确定 P/D 角色、启动 SGLang、注册 Router 和回收节点。

## 构建镜像

仓库不发布公共 worker 镜像。合作商必须提供已经通过 CUDA、Torch、SGLang 内部 fork、sgl-kernel、NIXL/UCX 和模型挂载验证的基础镜像：

```bash
docker build \
  --build-arg BASE_IMAGE=<partner-registry>/<approved-sglang-runtime>:<immutable-tag> \
  --build-arg TALEA_CUDA=12.8 \
  --build-arg TALEA_PYTHON=3.10 \
  --build-arg TALEA_SGLANG='<validated-sglang-version>' \
  -f deploy/container/Dockerfile \
  -t <partner-registry>/talea-worker:<immutable-tag> .
docker push <partner-registry>/talea-worker:<immutable-tag>
```

基础镜像必须提供 `python3`、实际的 `sglang.launch_server`，以及与 Chart `runtime` 配置一致的 `/opt/tai-talea/venv` 和 `/opt/tai-talea/wheelhouse`。模型权重不放进镜像，使用 `cache.existingClaim` 挂载到 `model.path`（默认 `/cache/models/glm5.2`）。

## 安装 Chart

```bash
kubectl -n inference create secret generic talea-worker-auth \
  --from-literal=token='<CONTROL_PLANE_BOOTSTRAP_TOKEN>'
helm lint deploy/helm/talea-worker --set bootstrap.token=lint-only-placeholder
helm template worker-p deploy/helm/talea-worker \
  -f deploy/helm/talea-worker/values-h20.example.yaml \
  --set image.repository=<partner-registry>/talea-worker \
  --set image.tag=<immutable-tag> \
  --set bootstrap.tokenSecretName=talea-worker-auth \
  --set cache.existingClaim=<model-pvc> \
  --set worker.controlEndpoint=http://talea-control.talea.svc:8787
helm upgrade --install worker-p deploy/helm/talea-worker -n inference --create-namespace \
  -f deploy/helm/talea-worker/values-h20.example.yaml \
  --set image.repository=<partner-registry>/talea-worker \
  --set image.tag=<immutable-tag> \
  --set bootstrap.tokenSecretName=talea-worker-auth \
  --set cache.existingClaim=<model-pvc> \
  --set worker.role=prefill \
  --set worker.controlEndpoint=http://talea-control.talea.svc:8787
```

为 Decode 节点再部署一个独立 release，改用 `worker.role=decode`。通过 Talea 正式纳管时建议留空 `worker.role`，由控制面统一分配 P/D。

## H20/H200 验收

先确认 NVIDIA device plugin 标签、驱动版本和 GPU：

```bash
kubectl get node --show-labels | findstr /i "H20 H200 nvidia"
kubectl -n inference exec deploy/worker-p -- nvidia-smi
kubectl -n inference exec deploy/worker-p -- python3 -m tai_talea_bootstrap check --profile /etc/tai-talea/profile.json
```

验收必须同时覆盖 CUDA/驱动兼容、`torch.cuda.is_available()`、SGLang 内部补丁、NIXL/UCX 数据通道、模型目录、9001 鉴权、9002 服务端口、P/D 互通，以及控制节点通过 `/bootstrap/start` 后的真实推理。H20/H200 不能仅凭 GPU 名称通过验收，应记录镜像 digest、驱动、CUDA、Torch、SGLang、sgl-kernel、NIXL 和模型哈希。

## 运行约定

- `9001` 只给控制节点访问，所有请求都带 `X-Bootstrap-Token`。
- `9002` 是 SGLang 服务端点，只有 Talea 启动成功并完成 Router 注册后才是可服务端点。
- `8998` 是 P/D disaggregation 端口，按实际 SGLang 参数和网络策略放通。
- Chart readiness 只表示 bootstrap 可鉴权访问，不等于 P/D 已上线；看板应以 Talea 实例状态和 Router membership 为准。
- 回收由控制节点执行，不能直接删除 Pod 代替摘流和租约释放。
