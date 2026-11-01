#!/usr/bin/env python3
"""Close the Router's routing to every worker, then wait for in-flight to finish.

Why not the control plane's drain endpoint: it waits for load to reach zero and
then stops SGLang, but it only closes the Router's readiness when the instance
has no recorded worker id yet. For a normal serving instance the Router keeps
routing until its own health check fails - minutes later, and every request in
between goes to a process that is on its way out. The one primitive that makes
the Router stop choosing a worker is PUT /state/workers/{id}, which is what this
script calls.

Closing readiness does not disturb requests already in flight: the Router stops
choosing the worker for new requests, and an open stream keeps its connection.
That is why the wait for in-flight to reach zero comes after the close, not
before.

Runs inside the cluster (the Router's control port is a ClusterIP). Prints a
final marker so the caller can decide without parsing prose.

MODE=close (default) closes routing and waits for in-flight to finish.
MODE=open puts the workers back, which is the only way to undo a close: the
control plane reads the Router's readiness but never writes it back on its own,
so a hand-drained pool would stay unroutable until the Pods were recreated.

exit 0  closed and quiesced, or reopened
exit 2  at least one worker refused the change - the pool is half changed
exit 3  requests still running when the quiesce window expired
exit 1  the Router never answered
"""

import json
import os
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("ROUTER_URL", "http://talea-router:30001").rstrip("/")
QUIESCE_TIMEOUT = int(os.environ.get("QUIESCE_TIMEOUT", "90"))
# Closing routing only changes how the *next* request is placed. Requests that
# were dispatched microseconds earlier are already at the worker, and the load
# signal the Router exposes (/get_load) counts what is queued there, not what is
# generating - four concurrent requests were observed reading as zero. So the
# wait is a settle window first, and the load probe only catches a worker that is
# genuinely backed up behind its queue.
SETTLE_SECONDS = int(os.environ.get("SETTLE_SECONDS", "20"))
POLL_SECONDS = 3
# close: stop routing (before a scale-down). open: put the workers back, which is
# the only way to undo a drain without restarting the Pods - the control plane
# reads readiness but never sets it back itself.
MODE = os.environ.get("MODE", "close").strip().lower()


def call(method, path, payload=None, timeout=10):
    data = json.dumps(payload).encode("utf-8") if payload is not None else None
    request = urllib.request.Request(
        BASE + path,
        data=data,
        method=method,
        headers={"Accept": "application/json", "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=timeout) as response:
        body = response.read().decode("utf-8", "replace")
    return json.loads(body) if body.strip() else {}


def main():
    workers = None
    for attempt in range(20):
        try:
            workers = call("GET", "/workers").get("workers") or []
            break
        except (urllib.error.URLError, OSError, ValueError) as exc:
            print("router not ready (%s), retry %d" % (exc, attempt + 1))
            time.sleep(3)
    if workers is None:
        print("DRAIN_FAILED: %s never answered /workers" % BASE)
        return 1

    if not workers:
        print("no workers registered; nothing to drain")
        print("QUIESCE_OK")
        return 0

    print("workers:")
    for worker in workers:
        print("  %-40s %-8s healthy=%s load=%s"
              % (worker.get("id"), worker.get("worker_type"),
                 worker.get("is_healthy"), worker.get("load")))

    # Only the routable ones matter. The registry keeps entries for Pods that are
    # long gone (a worker id is per registration, not per address), and reviving
    # one of those on the way back up would send traffic to a dead URL until the
    # health check gave up. is_healthy is what the Router itself would use.
    targets = [w for w in workers if w.get("is_healthy")]
    skipped = len(workers) - len(targets)
    if skipped:
        print("skipping %d worker(s) the Router already considers unhealthy" % skipped)

    failed = []
    state = "ready" if MODE == "open" else "unavailable"
    for worker in targets:
        worker_id = worker.get("id")
        if not worker_id:
            continue
        try:
            # reason is a closed enum on the Router side (manual | health_check);
            # a free-text explanation is a 422, not a comment.
            call("PUT", "/state/workers/%s" % worker_id,
                 {"state": state, "reason": "manual"})
            print("  %s -> %s" % (worker_id, state))
        except (urllib.error.URLError, OSError, ValueError) as exc:
            failed.append(worker_id)
            print("  %s -> FAILED: %s" % (worker_id, exc))

    if failed:
        print("DRAIN_PARTIAL: %d of %d workers refused the change" % (len(failed), len(targets)))
        return 2

    if not targets:
        print("no routable workers; nothing to change")
        print("QUIESCE_OK")
        return 0

    if MODE == "open":
        print("ROUTER_OPEN")
        return 0

    deadline = time.time() + QUIESCE_TIMEOUT
    print("settling for %ds before reading load" % SETTLE_SECONDS)
    time.sleep(SETTLE_SECONDS)

    total = 0
    unknown = 0
    while time.time() < deadline:
        try:
            loads = call("GET", "/get_loads").get("loads") or []
            # A negative load means the Router could not read it. Treating that
            # as idle would declare quiescence on no evidence; treating it as
            # busy would hang until the deadline. Count both and decide below.
            values = [entry.get("load") for entry in loads if entry.get("load") is not None]
            total = sum(value for value in values if value > 0)
            unknown = len([value for value in values if value < 0])
        except (urllib.error.URLError, OSError, ValueError) as exc:
            print("load probe failed: %s" % exc)
            total, unknown = 0, 1
        print("in-flight: %d, unreadable: %d" % (total, unknown))
        if total == 0 and unknown == 0:
            print("QUIESCE_OK")
            return 0
        time.sleep(POLL_SECONDS)

    if total == 0:
        # Nothing measurable was running for the whole window; the requests that
        # were in flight when routing closed have had their time.
        print("QUIESCE_OK: no measurable load after %ds (load unreadable on %d worker(s))"
              % (QUIESCE_TIMEOUT, unknown))
        return 0
    print("QUIESCE_TIMEOUT: %d token(s) still running after %ds" % (total, QUIESCE_TIMEOUT))
    return 3


if __name__ == "__main__":
    sys.exit(main())
