# Talea worker runtime layer. The partner supplies the CUDA/SGLang base image.
# The image address is deliberately a build argument because this repository
# cannot publish the internal SGLang fork, model weights, CUDA driver matrix,
# and NIXL runtime.
#
# docker build --build-arg BASE_IMAGE=<approved-registry>/sglang-runtime:<tag> \
#   -f deploy/container/Dockerfile .

ARG BASE_IMAGE
FROM ${BASE_IMAGE}

USER root
WORKDIR /opt/tai-talea

ARG TALEA_CUDA=12.8
ARG TALEA_PYTHON=3.10
ARG TALEA_SGLANG=0.0.0+g5a26fc1f.cu12
ARG TALEA_MODEL_ID=glm5.2

COPY bootstrap/tai_talea_bootstrap /opt/tai-talea/bootstrap/tai_talea_bootstrap
COPY deploy/container/profile.json /etc/tai-talea/profile.json
COPY deploy/container/entrypoint.sh /usr/local/bin/tai-talea-entrypoint

ENV PYTHONPATH=/opt/tai-talea/bootstrap \
    PYTHONUNBUFFERED=1 \
    TAI_TALEA_ROOT=/opt/tai-talea \
    TAI_TALEA_PROFILE=/etc/tai-talea/profile.json \
    TAI_TALEA_BOOTSTRAP_HOST=0.0.0.0 \
    TAI_TALEA_BOOTSTRAP_PORT=9001 \
    TAI_TALEA_SERVICE_PORT=9002

RUN command -v bash \
    && command -v python3 \
    && test -x /opt/tai-talea/venv/bin/python \
    && test -f /opt/tai-talea/wheelhouse/requirements.txt \
    && TALEA_PROFILE_CUDA="${TALEA_CUDA}" \
       TALEA_PROFILE_PYTHON="${TALEA_PYTHON}" \
       TALEA_PROFILE_SGLANG="${TALEA_SGLANG}" \
       TALEA_PROFILE_MODEL="${TALEA_MODEL_ID}" \
       python3 -c 'import json, os; p="/etc/tai-talea/profile.json"; d=json.load(open(p, encoding="utf-8")); d.update(cuda=os.environ["TALEA_PROFILE_CUDA"], python=os.environ["TALEA_PROFILE_PYTHON"], sglang=os.environ["TALEA_PROFILE_SGLANG"], model_id=os.environ["TALEA_PROFILE_MODEL"]); json.dump(d, open(p, "w", encoding="utf-8"), indent=2); open(p, "a", encoding="utf-8").write("\n")' \
    && python3 -m py_compile /opt/tai-talea/bootstrap/tai_talea_bootstrap/*.py \
    && install -d -m 0750 /opt/tai-talea/logs /opt/tai-talea/run /etc/tai-talea \
    && chmod 0755 /usr/local/bin/tai-talea-entrypoint

EXPOSE 9001 9002 8998

# Talea calls this authenticated API. It assigns P/D and starts SGLang later.
ENTRYPOINT ["/usr/local/bin/tai-talea-entrypoint"]
CMD ["serve"]
