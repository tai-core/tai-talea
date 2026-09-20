# Talea worker container

This image packages the repository's Python bootstrap on top of a partner supplied CUDA/SGLang runtime. It exposes the authenticated bootstrap API on port `9001`; the SGLang service uses the worker port supplied by Talea when it sends `/bootstrap/start` (the image default is `9002`, matching the SSH onboarding contract).

## Build

The base image is intentionally supplied by the partner. It must contain the
validated CUDA/Torch/SGLang/NIXL runtime, `/opt/tai-talea/venv/bin/python` and
the locked wheelhouse:

```sh
docker build \
  --build-arg BASE_IMAGE=<partner-registry>/<approved-sglang-runtime>:<immutable-tag> \
  --build-arg TALEA_CUDA=12.8 \
  --build-arg TALEA_PYTHON=3.10 \
  --build-arg TALEA_SGLANG='<validated-sglang-version>' \
  -t <your-registry>/tai-talea-worker:<tag> \
  -f deploy/container/Dockerfile .
```

`<your-registry>` and the runtime tag are placeholders; this repository does not assume a partner's registry or publish an image on its behalf.

## Run

The bootstrap refuses to start without a secret. The control plane must use the same value as its `controller.bootstrap_token`:

```sh
docker run --rm --gpus all \
  -e TAI_TALEA_BOOTSTRAP_TOKEN='<random-secret-at-least-16-chars>' \
  -e TALEA_MODEL_ID='glm5.2' \
  -e TALEA_MODEL_PATH='/models/glm5.2' \
  -v /path/to/models:/models:ro \
  -p 9001:9001 -p 9002:9002 -p 8998:8998 \
  <your-registry>/tai-talea-worker:<tag>
```

Leave `TALEA_ROLE` empty for normal Talea ownership. Set it to `prefill` or `decode` only when the container should start that role itself; the control plane can then adopt the matching running service. The entrypoint also supports `health` and `stop` subcommands for orchestrator probes and graceful termination.

The container logs `TALEA_GPU_ARCH` and `TALEA_CONTROL_ENDPOINT` when supplied. GPU selection remains the platform's responsibility (`--gpus` for Docker or `nvidia.com/gpu` in Kubernetes). Capacity registration still belongs to the partner Push/SSH onboarding flow so leases and signatures remain under control-plane policy.

## Helm

`deploy/helm/talea-worker` publishes the same ports (`9001` bootstrap, `9002` SGLang, `8998` PD bootstrap), injects the bootstrap token from a Secret, and creates a model/cache PVC by default. Supply a real image, token, and model claim before installing:

```sh
helm install talea-worker deploy/helm/talea-worker \
  --set image.repository=<approved-registry>/tai-talea-worker \
  --set image.tag=<immutable-tag> \
  --set bootstrap.token='<random-secret-at-least-16-chars>'
```

Leave `worker.role` empty for Planner assignment. Setting it to `prefill` or `decode` makes the entrypoint start that role locally, but it still does not create a Talea lease; submit the capacity through the partner Push/SSH onboarding flow.
