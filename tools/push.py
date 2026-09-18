#!/usr/bin/env python3
"""Push signed capacity events to a tai-talea control plane (test harness).

Standard library only. This is the partner-side simulation used to drive the
Push API: signed ADDED / REVOKED events for the static test containers.

Signing contract (internal/api/auth.go):
    Authorization:        Bearer <push_token>
    X-Capacity-Partner:   <partner id>
    X-Capacity-Timestamp: unix seconds
    X-Capacity-Signature: hex HMAC-SHA256(secret, "<timestamp>.<body>")

Examples:

    python tools/push.py add --id container-prefill \\
        --endpoint http://120.220.102.21:30086 \\
        --service-endpoint http://172.19.4.58:9002 \\
        --lease lease-prefill-001 --gpu H100 --gpu-count 8 \\
        --model Qwen/Qwen2.5-3B-Instruct

    python tools/push.py revoke --id container-prefill

Event ids are deterministic ("<kind>-<instance id>") so re-running the same
command exercises the idempotent-duplicate path instead of creating churn.
"""

from __future__ import annotations

import argparse
import hashlib
import hmac
import json
import sys
import time
import urllib.error
import urllib.request

MODEL_DEFAULT = "Qwen/Qwen2.5-3B-Instruct"


def sign(secret: str, timestamp: str, body: bytes) -> str:
    payload = timestamp.encode() + b"." + body
    return hmac.new(secret.encode(), payload, hashlib.sha256).hexdigest()


def post(base: str, args: argparse.Namespace, body: dict) -> int:
    raw = json.dumps(body, separators=(",", ":")).encode()
    timestamp = str(int(time.time()))
    request = urllib.request.Request(
        base.rstrip("/") + "/v1/capacity/events",
        data=raw,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Authorization": "Bearer " + args.token,
            "X-Capacity-Partner": args.partner,
            "X-Capacity-Timestamp": timestamp,
            "X-Capacity-Signature": sign(args.secret, timestamp, raw),
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            print(response.status, response.read().decode())
            return 0
    except urllib.error.HTTPError as error:
        print(error.code, error.read().decode(), file=sys.stderr)
        return 2


def instance_payload(args: argparse.Namespace) -> dict:
    payload: dict = {"id": args.id, "endpoint": args.endpoint}
    if args.service_endpoint:
        payload["service_endpoint"] = args.service_endpoint
    if args.lease:
        payload["lease_id"] = args.lease
    spec: dict = {}
    if args.gpu:
        spec["gpu"] = args.gpu
    if args.gpu_count:
        spec["gpu_count"] = args.gpu_count
    if args.model:
        spec["model_support"] = [args.model]
    if spec:
        payload["spec"] = spec
    return payload


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--base", default="http://127.0.0.1:18080")
    parser.add_argument("--partner", default="jiuzhang")
    parser.add_argument("--token", default="jiuzhang-push-token-0123")
    parser.add_argument("--secret", default="jiuzhang-push-secret-0123456789abcdef")
    sub = parser.add_subparsers(dest="kind", required=True)

    for kind in ("add", "update", "revoke"):
        one = sub.add_parser(kind)
        one.add_argument("--id", required=True)
        if kind != "revoke":
            one.add_argument("--endpoint", required=True)
            one.add_argument("--service-endpoint")
        one.add_argument("--lease")
        one.add_argument("--gpu", default="H100")
        one.add_argument("--gpu-count", type=int, default=8)
        one.add_argument("--model", default=MODEL_DEFAULT)
        one.add_argument("--event-id")

    args = parser.parse_args(argv)
    event_id = args.event_id or "%s-%s" % (args.kind, args.id)
    body: dict = {
        "event_id": event_id,
        "partner_id": args.partner,
        "type": {"add": "CAPACITY_ADDED", "update": "CAPACITY_UPDATED", "revoke": "CAPACITY_REVOKED"}[args.kind],
        "occurred_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    if args.kind == "revoke":
        body["grace_seconds"] = 30
    else:
        body["instance"] = instance_payload(args)
    return post(args.base, args, body)


if __name__ == "__main__":
    raise SystemExit(main())
