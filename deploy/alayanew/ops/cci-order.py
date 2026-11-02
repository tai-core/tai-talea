#!/usr/bin/env python3
"""Order one CCI instance and wait for it to land (plan stage C / G1 entry).

Wraps alaya_client.call with retries - the platform API path here has shown
transient transport failures (proxy 502, DNS flakes), and a create that is
never answered must not be blindly repeated: the name is the idempotency
handle. On a transport failure the create is retried only while the instance
name is still absent from the list.

Usage:
    python cci-order.py --name glm53-pd2 --nas-storage-id <uuid>
    python cci-order.py --name glm53-pd3 --nas-storage-id <uuid> --poll-only

Credentials come from the environment (TALEA_ALAYA_AK / TALEA_ALAYA_SK),
see alaya_client.py. TALEA_ALAYA_DIRECT=1 forces a no-proxy connection.
"""
import argparse
import json
import sys
import time

from alaya_client import call

SKU_8CARD = "skus.cci.gpu.h100.80gb.hbm3.b11.a2.5.11.s20a46b:2"
IMAGE = "registry.hd-02.alayanew.com:8443/alayanew-public/sglang:20250930"
RESULT_PATH = "/api/osm/v1/subscription/sku/cci/result/{}"


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


def call_retry(method, path, query="", body=None, tries=5, what=""):
    """Transport failures are transient here; a 4xx/5xx body is NOT retried."""
    last = None
    for n in range(1, tries + 1):
        code, d = call(method, path, query, body=body)
        if code != -1:
            return code, d
        last = d
        log(f"{what}: transport error ({d}), retry {n}/{tries}")
        time.sleep(3)
    raise SystemExit(f"{what}: transport kept failing: {last}")


def find_instance(name):
    code, d = call_retry("GET", "/api/osm/v1/cci/instance/list",
                         f"pageNo=1&pageSize=50&name={name}", what="instance list")
    if code != 200 or not isinstance(d, dict):
        raise SystemExit(f"instance list failed: HTTP {code} {json.dumps(d, ensure_ascii=False)[:200]}")
    recs = (d.get("data") or {}).get("records") or []
    for r in recs:
        if r.get("name") == name:
            return r
    return None


def create(args):
    body = {
        "productCode": args.product_code,
        "cciParams": {
            "name": args.name,
            "image": args.image,
            "description": "managed by tai-talea",
            "computeResourceType": args.compute,
            "priority": args.priority,
        },
    }
    if args.nas_storage_id:
        body["cciParams"]["storageConfigEnable"] = True
        body["cciParams"]["storageConfigs"] = [{
            "storageId": args.nas_storage_id,
            "storageType": "nas-capacity",
            "fileDirectory": args.nas_dir,
            "mountPath": args.mount_path,
            "onlyRead": False,
        }]
    code, d = call_retry("POST", "/api/osm/v1/subscription/CCI/create", body=body, what="create")
    log(f"create -> HTTP {code} {json.dumps(d, ensure_ascii=False)[:200]}")
    if not (isinstance(d, dict) and d.get("status") == 200):
        raise SystemExit(f"create refused: {json.dumps(d, ensure_ascii=False)[:300]}")
    return d.get("data")


def poll_result_key(key, timeout_s):
    """The subscription result flips from QUEUED to a terminal state."""
    deadline = time.time() + timeout_s
    last = None
    while time.time() < deadline:
        code, d = call_retry("GET", RESULT_PATH.format(key), what="subscribe result")
        state = json.dumps(d.get("data"), ensure_ascii=False)[:160] if isinstance(d, dict) else str(d)[:160]
        if state != last:
            log(f"subscribe result: {state}")
            last = state
        s = json.dumps(d)
        if '"SUCCESS"' in s or '"FAILED"' in s or '"ERROR"' in s:
            return d
        time.sleep(5)
    raise SystemExit("subscribe result did not settle in time")


def wait_running(name, timeout_s):
    deadline = time.time() + timeout_s
    last = None
    while time.time() < deadline:
        rec = find_instance(name)
        if rec:
            state = f"{rec.get('status')}/{rec.get('subStatus')}"
            if state != last:
                log(f"instance {name}: {state}")
                last = state
            if rec.get("status") == "RUNNING":
                return rec
            if rec.get("status") in ("FAILED", "RELEASED", "ERROR"):
                raise SystemExit(f"instance {name} reached terminal {rec.get('status')}")
        time.sleep(10)
    raise SystemExit(f"instance {name} did not reach RUNNING in time")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--name", required=True, help="instance name (<=20 chars)")
    ap.add_argument("--product-code", default=SKU_8CARD)
    ap.add_argument("--image", default=IMAGE)
    ap.add_argument("--compute", default="EXCLUSIVE", choices=["ELASTIC", "EXCLUSIVE"])
    ap.add_argument("--priority", type=int, default=1)
    ap.add_argument("--nas-storage-id", default="", help="NAS volume UUID; omit to order without storage")
    ap.add_argument("--nas-dir", default="glm53-weights")
    ap.add_argument("--mount-path", default="/root/gongji")
    ap.add_argument("--poll-only", action="store_true",
                    help="an order with this name already exists - just wait for RUNNING")
    ap.add_argument("--wait-seconds", type=int, default=900)
    args = ap.parse_args()

    if len(args.name) > 20:
        raise SystemExit("instance names are capped at 20 characters by the platform")

    existing = find_instance(args.name)
    if existing and not args.poll_only:
        raise SystemExit(f"instance {args.name} already exists ({existing.get('status')}); "
                         "use --poll-only to wait for it, or pick another name")
    if not existing and not args.poll_only:
        key = create(args)
        if key:
            poll_result_key(key, args.wait_seconds)
    rec = wait_running(args.name, args.wait_seconds)
    log(f"RUNNING: {args.name} id={rec.get('id')}")
    print(json.dumps(rec, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
