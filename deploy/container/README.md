# Talea worker container

This image packages the repository's Python bootstrap on top of a partner supplied CUDA/SGLang runtime. It exposes the authenticated bootstrap API on port `9001`; the SGLang service uses the worker port supplied by Talea when it sends `/bootstrap/start` (the image default is `9002`, matching the SSH onboarding contract).

## Build

Two shapes are supported. Which one applies is decided by whether the base
image already carries a runtime.

**The base image supplies the runtime.** It must contain the validated
CUDA/Torch/SGLang runtime, `/opt/tai-talea/venv/bin/python` and the locked
wheelhouse:

```sh
docker build \
  --build-arg BASE_IMAGE=<partner-registry>/<approved-sglang-runtime>:<immutable-tag> \
  -t <your-registry>/tai-talea-worker:<tag> \
  -f deploy/container/Dockerfile .
```

**This repository supplies the runtime.** Pass `WHEELHOUSE_DIR` -- a path inside
the build context, produced and verified by `provision/wheelhouse` -- and the
image installs the locked runtime from it with no package index in reach:

```sh
# cwd is the repository root
docker build \
  --build-arg BASE_IMAGE=<bare cuda image> \
  --build-arg WHEELHOUSE_DIR=provision/wheelhouse/out \
  -t <your-registry>/tai-talea-worker:<tag> \
  -f deploy/container/Dockerfile .
```

The install itself is `deploy/container/runtime-layer.sh`, which also asserts
the postconditions, writes the loader configuration the CUDA 12 mooncake wheels
need, and restores the `lib64`/versionless `.so` links SGLang's JIT asks the
linker for. Read that script before changing the base image.

`<your-registry>` and the runtime tag are placeholders; this repository does not assume a partner's registry or publish an image on its behalf.

## Run

The bootstrap refuses to start without a secret. The control plane must use the same value as its `controller.bootstrap_token`:

```sh
docker run --rm --gpus all \
  -e TAI_TALEA_BOOTSTRAP_TOKEN='<random-secret-at-least-16-chars>' \
  -e TALEA_MODEL_ID='zai-org/GLM-5.3-Flash' \
  -e TALEA_MODEL_PATH='/models/GLM-5.3-Flash' \
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
