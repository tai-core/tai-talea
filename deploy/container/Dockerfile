# Talea worker runtime image.
#
# Two supported shapes, because the two platforms in use differ in exactly this
# way:
#
#   partner-supplied runtime layer
#     The base image already carries a verified /opt/tai-talea/{venv,wheelhouse}.
#     Pass BASE_IMAGE alone and nothing else changes.
#
#   self-built runtime layer
#     The base image is a bare CUDA image. Pass WHEELHOUSE_DIR (a path inside
#     this build context, produced by provision/wheelhouse) and the image
#     installs the locked runtime from that offline bundle with no package
#     index in reach:
#
#       (cwd = repository root)
#       docker build -f deploy/container/Dockerfile \
#         --build-arg BASE_IMAGE=<bare cuda image> \
#         --build-arg WHEELHOUSE_DIR=provision/wheelhouse/out \
#         -t <registry>/talea-worker:<tag> .
#
#     The bundle is never assembled by hand: provision/wheelhouse builds and
#     verifies it (build-env.sh -> build.py -> verify.py).

ARG BASE_IMAGE
FROM ${BASE_IMAGE}

USER root
WORKDIR /opt/tai-talea

ARG TALEA_CUDA=13.x
ARG TALEA_PYTHON=3.12
ARG TALEA_SGLANG=0.5.20
ARG TALEA_MODEL_ID=zai-org/GLM-5.3-Flash
# Empty by default so the partner-runtime shape keeps working unchanged.
ARG WHEELHOUSE_DIR=deploy/container/empty

COPY bootstrap/tai_talea_bootstrap /opt/tai-talea/bootstrap/tai_talea_bootstrap
COPY provision/sglang_compat.py /opt/tai-talea/onboarding/sglang_compat.py
COPY deploy/container/runtime-layer.sh /usr/local/bin/talea-runtime-layer
COPY deploy/container/entrypoint.sh /usr/local/bin/tai-talea-entrypoint
COPY deploy/container/profile.json /etc/tai-talea/profile.json
COPY ${WHEELHOUSE_DIR} /opt/tai-talea/wheelhouse

ENV PYTHONPATH=/opt/tai-talea/bootstrap \
    PYTHONUNBUFFERED=1 \
    TAI_TALEA_ROOT=/opt/tai-talea \
    TAI_TALEA_PROFILE=/etc/tai-talea/profile.json \
    TAI_TALEA_BOOTSTRAP_HOST=0.0.0.0 \
    TAI_TALEA_BOOTSTRAP_PORT=9001 \
    TAI_TALEA_SERVICE_PORT=9002

# The runtime layer, its postconditions and the loader configuration all live
# in one script: a mid-RUN `#` comment inside a line-continuation block would
# comment out the rest of the joined command, and neither a broken base image
# nor a half-installed bundle should survive the build.
RUN chmod 0755 /usr/local/bin/talea-runtime-layer \
    && TALEA_PYTHON="${TALEA_PYTHON}" /usr/local/bin/talea-runtime-layer

RUN set -eu; \
    TALEA_PROFILE_CUDA="${TALEA_CUDA}" \
    TALEA_PROFILE_PYTHON="${TALEA_PYTHON}" \
    TALEA_PROFILE_SGLANG="${TALEA_SGLANG}" \
    TALEA_PROFILE_MODEL="${TALEA_MODEL_ID}" \
    python3 -c 'import json, os; p="/etc/tai-talea/profile.json"; d=json.load(open(p, encoding="utf-8")); d.update(cuda=os.environ["TALEA_PROFILE_CUDA"], python=os.environ["TALEA_PROFILE_PYTHON"], sglang=os.environ["TALEA_PROFILE_SGLANG"], model_id=os.environ["TALEA_PROFILE_MODEL"]); json.dump(d, open(p, "w", encoding="utf-8"), indent=2); open(p, "a", encoding="utf-8").write("\n")'; \
    python3 -m py_compile /opt/tai-talea/bootstrap/tai_talea_bootstrap/*.py; \
    install -d -m 0750 /opt/tai-talea/logs /opt/tai-talea/run /etc/tai-talea; \
    chmod 0755 /usr/local/bin/tai-talea-entrypoint

EXPOSE 9001 9002 8998

# Talea calls this authenticated API. It assigns P/D and starts SGLang later.
ENTRYPOINT ["/usr/local/bin/tai-talea-entrypoint"]
CMD ["serve"]
