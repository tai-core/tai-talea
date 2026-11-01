#!/usr/bin/env python3
"""Emit the worker Deployments for the aggregated B300 pool.

The control plane adopts these Pods through its pull snapshot, so the labels
matter more than the containers do: app.kubernetes.io/managed-by=tai-talea and
tai-talea.partner=<partner> are the selector the Kubernetes partner lists with.
A worker without them is invisible capacity.

The bootstrap token is read from the environment rather than written here: it is
a shared secret, config.yaml carries the same value, and this repository is
public. `apply.sh` sources it from the same file it uses to render config.yaml.

usage: TALEA_BOOTSTRAP_TOKEN=... gen-workers.py [output-dir]
"""

import io
import os
import sys

NAMESPACE = os.environ.get("TALEA_NAMESPACE", "kunhaoyuan")
PARTNER = os.environ.get("TALEA_PARTNER", "kunhaoyuan")
IMAGE = os.environ.get(
    "TALEA_WORKER_IMAGE",
    "kunhaoyuan-62c8ab7d.ecis.jinan-1.cmecloud.cn/khy/gongji:talea-worker-0.9.2",
)
TOKEN = os.environ.get("TALEA_BOOTSTRAP_TOKEN", "")
MODEL_MOUNT = "/models"
# The cache and log directories still carry the old product name. Renaming them
# would throw away a warm Triton/compile cache, which is the difference between
# a 5 minute and an 11 minute cold start, so the paths stay as they are.
NODES = {
    "7": "sdsc-aic-mdmz-pm-os01-aicube-7",
    "8": "sdsc-aic-mdmz-pm-os01-aicube-8",
}


TEMPLATE = """---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {name}
  namespace: {namespace}
  labels:
    app: talea-worker
    worker-id: {name}
spec:
  replicas: 1
  # The Pod is Ready as soon as the bootstrap answers, which is minutes before
  # the model is loaded; a rollout never waits for the weights.
  progressDeadlineSeconds: 1800
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: talea-worker
      worker-id: {name}
  template:
    metadata:
      labels:
        app: talea-worker
        worker-id: {name}
        app.kubernetes.io/managed-by: tai-talea
        tai-talea.partner: {partner}
    spec:
      nodeSelector:
        kubernetes.io/hostname: {host}
      imagePullSecrets:
        - name: harbor-cred
      # SGLang stops the model on SIGTERM; 60 s lets it exit instead of being
      # killed with four cards still held.
      terminationGracePeriodSeconds: 60
      containers:
        - name: worker
          image: {image}
          imagePullPolicy: Always
          env:
            - name: TAI_TALEA_BOOTSTRAP_TOKEN
              value: "{token}"
            - name: TAI_TALEA_BOOTSTRAP_HOST
              value: "0.0.0.0"
            - name: TAI_TALEA_BOOTSTRAP_PORT
              value: "9001"
            - name: TAI_TALEA_SERVICE_PORT
              value: "9002"
            - name: TAI_TALEA_PROFILE
              value: /etc/tai-talea/profile.json
          ports:
            - name: bootstrap
              containerPort: 9001
            - name: service
              containerPort: 9002
          readinessProbe:
            httpGet:
              path: /bootstrap/health
              port: 9001
              httpHeaders:
                - name: X-Bootstrap-Token
                  value: "{token}"
            initialDelaySeconds: 5
            periodSeconds: 10
            timeoutSeconds: 5
            failureThreshold: 6
          # Same probe the Kubernetes partner would install: a process tree
          # that stays alive but stops answering is restarted in place, keeping
          # the Pod, its address and its lease.
          livenessProbe:
            httpGet:
              path: /bootstrap/liveness
              port: 9001
              httpHeaders:
                - name: X-Bootstrap-Token
                  value: "{token}"
            initialDelaySeconds: 60
            periodSeconds: 10
            timeoutSeconds: 5
            failureThreshold: 6
          resources:
            requests:
              cpu: "32"
              memory: 128Gi
              nvidia.com/gpu: "4"
            limits:
              cpu: "100"
              memory: 512Gi
              nvidia.com/gpu: "4"
          volumeMounts:
            - name: weights
              mountPath: {model_mount}
              readOnly: true
            - name: compile-cache
              mountPath: /root/.cache
            - name: worker-logs
              mountPath: /logs
            - name: shm
              mountPath: /dev/shm
      volumes:
        - name: weights
          hostPath:
            path: /data02/model-weight
            type: Directory
        - name: compile-cache
          hostPath:
            path: /data02/talea-glm53/cache
            type: DirectoryOrCreate
        - name: worker-logs
          hostPath:
            path: /data02/talea-glm53/logs
            type: DirectoryOrCreate
        - name: shm
          emptyDir:
            medium: Memory
            sizeLimit: 16Gi
"""


def render(name, node_key):
    return TEMPLATE.format(
        name=name,
        namespace=NAMESPACE,
        partner=PARTNER,
        image=IMAGE,
        token=TOKEN,
        host=NODES[node_key],
        model_mount=MODEL_MOUNT,
    )


def write(path, pairs):
    text = "".join(render(name, node) for name, node in pairs)
    with io.open(path, "w", encoding="utf-8", newline="\n") as handle:
        handle.write(text)
    print("%s: %s" % (os.path.basename(path), ", ".join(name for name, _ in pairs)))


if __name__ == "__main__":
    if not TOKEN:
        sys.exit("set TALEA_BOOTSTRAP_TOKEN; it must match config.yaml's bootstrap_token")
    if len(sys.argv) < 2:
        # Never default to this directory: the rendered output carries the token
        # in plain text, and a stray `git add .` would publish it.
        sys.exit("usage: gen-workers.py <output-dir>")
    here = sys.argv[1]
    write(os.path.join(here, "50-workers-a.yaml"), [("talea-w0", "7"), ("talea-w1", "8")])
    write(os.path.join(here, "51-workers-b.yaml"), [("talea-w2", "7"), ("talea-w3", "8")])
