#!/usr/bin/env python3
"""Release leftover talea-* probe instances, and probe the subscription-result paths.

Two independent jobs:

  1. Sweep `talea-` instances that are not the managed workers and release them.
     A probe killed mid-flight (shell timeout, Ctrl-C at the wrong moment) never
     reaches its own finally block, so a sweeper is the only thing that makes the
     experiment safe to repeat. A leftover QUEUED instance also occupies one of
     the three instance slots, which turns the next order into a fake
     "quota exhausted" (40010).

  2. Test the documented replacement for the decommissioned subscription-result
     path. The old `/api/osm/v1/subscription/CCI/result/{key}` answers 212147
     ("接口已停用"); the live one is `/api/osm/v1/subscription/sku/cci/result/{key}`.
     The response shape differs too: `data.status` + `data.resources[]` with
     `resourceInstanceId` / `errorCode`, not `data.result`.

Run this before and after any capacity probe. `--sweep-only` skips job 2.

usage: cci-probe-sweep.py [--keep glm53-pd2,glm53-pd3] [--sweep-only]
"""
import argparse
import json
import sys
import time

from alaya_client import call

DEFAULT_KEEP = "glm53-pd2,glm53-pd3"


def sweep(keep):
    code, d = call("GET", "/api/osm/v1/cci/instance/list", "pageNo=1&pageSize=50")
    recs = (d.get("data") or {}).get("records") or []
    print(f"== instances: {len(recs)}")
    for r in recs:
        mark = "" if r.get("name") in keep else "  <- candidate"
        print(f"   {r.get('name'):<24} {str(r.get('status')):<9} "
              f"{r.get('computeResourceType')}{mark}")
    targets = [r for r in recs
               if r.get("name") not in keep and (r.get("name") or "").startswith("talea-")]
    for r in targets:
        print(f"-- releasing {r['name']} {r.get('id')}")
        c, res = call("POST", f"/api/osm/v1/cci/instance/{r.get('id')}/release", body={})
        print(f"   HTTP {c} {json.dumps(res, ensure_ascii=False)[:160]}")
    return targets


def probe_results(key):
    """Compare the dead path with the live one; needs a key, so it is optional."""
    if not key:
        print("\n== subscription result paths: skipped (pass --result-key to test) ==")
        print("   old: /api/osm/v1/subscription/CCI/result/<key>         -> 212147 已停用")
        print("   new: /api/osm/v1/subscription/sku/cci/result/<key>     -> 200 data.status/resources[]")
        return
    for path in (
        "/api/osm/v1/subscription/CCI/result/" + key,
        "/api/osm/v1/subscription/sku/cci/result/" + key,
    ):
        c, d = call("GET", path)
        print(f"{c}  {path}\n     {json.dumps(d, ensure_ascii=False)[:260]}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--keep", default=DEFAULT_KEEP, help="comma-separated names to keep")
    ap.add_argument("--sweep-only", action="store_true")
    # An explicit flag, not a positional: a result key contains colons, and the
    # old "last argv with >=3 colons" heuristic could never be reached - argparse
    # rejected the unknown positional before the heuristic ever ran.
    ap.add_argument("--result-key", default="", help="subscription result key to probe both result paths with")
    args = ap.parse_args()
    keep = {n for n in args.keep.split(",") if n}
    swept = sweep(keep)
    if swept:
        # Release is asynchronous; a second pass catches records that only
        # became visible after the first list call.
        time.sleep(5)
        sweep(keep)
    if not args.sweep_only:
        print("\n== subscription result paths ==")
        probe_results(args.result_key)
    return 0


if __name__ == "__main__":
    sys.exit(main())
