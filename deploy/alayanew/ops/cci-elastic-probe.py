#!/usr/bin/env python3
"""Elastic (ELASTIC) CCI probe: can we get a GPU on demand, and how long does it queue?

Purpose: measure whether an extra node can be obtained through the *elastic*
channel, and how long the platform queue takes. This is the one question the
2026-10-26 quota investigation could not settle by reading APIs: `remainingCount`
is a pool snapshot, and the only way to learn the tenant quota is to let the
platform refuse an order.

Findings from the 2026-10-26 run (kept here so nobody repeats it):
  * ELASTIC GPU never lands - 8 cards queued 300s, 1 card queued 150s;
  * ELASTIC CPU lands in 22 seconds - so the channel itself works;
  * the real quota is **instances**, capped at 3, and a QUEUED instance counts;
  * `remainingCount` moves when *other tenants* allocate, not when we order.

Safety rules baked in here:
  * The instance is released in a `finally` block, so a crash or Ctrl-C cannot
    leave 8 cards burning.
  * Poll every 5 s; the status timeline is the deliverable.
  * Run `cci-probe-sweep.py` before and after. A probe killed mid-flight never
    reaches its own finally block, and a leftover QUEUED instance will make the
    next order fail with 40010 "quota exhausted" - which looks like a finding
    but is only our own garbage.

⛔ The 2026-10-26 note that "not calling open-port keeps the control plane from
adopting the probe" was WRONG: CCI publishes container ports 9001 and 9002 by
default, without us asking. A probe that reaches RUNNING *is* visible to a pull
reconciler. Keep the queue window short, or stop the control plane first.

usage: cci-elastic-probe.py [--cards 8|4|1] [--queue-seconds 300] [--keep]
"""
import argparse
import json
import sys
import time
from datetime import datetime

from alaya_client import call

PRODUCTS = {
    1: "skus.cci.gpu.h100.80gb.hbm3.b11.a2.5.11.saa70bd:2",
    2: "skus.cci.gpu.h100.80gb.hbm3.b11.a2.5.11.s6518fa:2",
    4: "skus.cci.gpu.h100.80gb.hbm3.b11.a2.5.11.sf1564e:2",
    8: "skus.cci.gpu.h100.80gb.hbm3.b11.a2.5.11.s20a46b:2",
}
IMAGE = "registry.hd-02.alayanew.com:8443/alayanew-public/sglang:20250930"
AIDC = 2


def now():
    return datetime.now().strftime("%H:%M:%S")


def log(msg):
    print(f"[{now()}] {msg}", flush=True)


def stock():
    """remainingCount per SKU - lets us see whether an allocation moves it."""
    code, d = call("GET", "/api/osm/v1/product/list-for-cci", f"aidcId={AIDC}")
    out = {}
    if isinstance(d, dict) and isinstance(d.get("data"), list):
        for p in d["data"]:
            out[p.get("gpuCount")] = p.get("remainingCount")
    return out


def list_instances():
    code, d = call("GET", "/api/osm/v1/cci/instance/list", "pageNo=1&pageSize=50")
    recs = []
    if isinstance(d, dict) and isinstance(d.get("data"), dict):
        recs = d["data"].get("records") or []
    return recs


def find(name):
    for r in list_instances():
        if r.get("name") == name:
            return r
    return None


def release(inst_id):
    code, d = call("POST", f"/api/osm/v1/cci/instance/{inst_id}/release", body={})
    log(f"release {inst_id} -> HTTP {code} {json.dumps(d, ensure_ascii=False)[:200]}")
    return code, d


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--cards", type=int, default=8, choices=[0, 1, 2, 4, 8])
    ap.add_argument("--queue-seconds", type=int, default=300)
    ap.add_argument("--keep", action="store_true", help="do not release (debug only)")
    ap.add_argument("--compute", default="ELASTIC", choices=["ELASTIC", "EXCLUSIVE"])
    ap.add_argument("--priority", type=int, default=1)
    ap.add_argument("--product-code", default="", help="override the SKU (e.g. the CPU one)")
    ap.add_argument("--tag", default="probe", help="short tag in the instance name")
    args = ap.parse_args()

    product = args.product_code or PRODUCTS[args.cards]
    name = f"talea-{args.tag}-{datetime.now().strftime('%m%d%H%M%S')}"
    # Instance names are capped at 20 characters by the platform.
    if len(name) > 20:
        name = name[:20]
    before = stock()
    inst_id = None
    landed = None
    timeline = []

    log(f"probe name   = {name}")
    log(f"productCode  = {product} ({args.cards} card)")
    log(f"compute/prio = {args.compute} / {args.priority}")
    log(f"stock before = {json.dumps(before)}")

    body = {
        "productCode": product,
        "cciParams": {
            "name": name,
            "image": IMAGE,
            "description": "talea elastic probe, released automatically",
            "computeResourceType": args.compute,
            "priority": args.priority,
        },
    }
    try:
        code, d = call("POST", "/api/osm/v1/subscription/CCI/create", body=body)
        log(f"create -> HTTP {code} {json.dumps(d, ensure_ascii=False)[:300]}")
        if not (isinstance(d, dict) and d.get("status") == 200):
            # A refusal is the answer to the quota question; nothing to clean up.
            log("create REFUSED - this alone is the answer; nothing to clean up")
            return 0
        key = d.get("data")
        log(f"result key   = {key}")

        deadline = time.time() + args.queue_seconds
        last = None
        while time.time() < deadline:
            rec = find(name)
            if rec:
                inst_id = rec.get("id")
                state = f"{rec.get('status')}/{rec.get('subStatus')}"
                if state != last:
                    timeline.append((now(), state))
                    log(f"status = {state}")
                    last = state
                if rec.get("status") == "RUNNING":
                    landed = rec
                    break
                if rec.get("status") in ("FAILED", "RELEASED", "ERROR"):
                    log(f"terminal status {rec.get('status')} - creation failed")
                    break
            else:
                # The list is eventually consistent right after submit: the
                # record can take a few seconds to show up.
                if last is None:
                    log("not listed yet")
                    last = "absent"
            time.sleep(5)

        if landed:
            after = stock()
            log(f"stock after  = {json.dumps(after)}")
            log(f"LANDED after {len(timeline)} transitions: "
                f"{json.dumps(landed.get('resource'), ensure_ascii=False)}")
            log(f"instance id  = {inst_id}")
        else:
            after = stock()
            log(f"still not RUNNING after {args.queue_seconds}s - status timeline above")
            log(f"stock after  = {json.dumps(after)}")
    finally:
        if inst_id and not args.keep:
            log("cleaning up")
            release(inst_id)
            time.sleep(3)
            gone = find(name) is None
            log(f"confirmed gone = {gone}")
        elif not inst_id:
            # Creation may have resolved after we gave up polling: look once more.
            rec = find(name)
            if rec and not args.keep:
                log("late record found during cleanup, releasing")
                release(rec.get("id"))
            elif rec:
                log(f"LEFTOVER (--keep): {rec.get('id')} {rec.get('name')} {rec.get('status')}")

    log("timeline: " + " | ".join(f"{t}={s}" for t, s in timeline))
    return 0


if __name__ == "__main__":
    sys.exit(main())
