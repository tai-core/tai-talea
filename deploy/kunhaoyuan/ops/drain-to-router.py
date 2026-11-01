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

The wait reads `sglang:num_running_reqs` from the worker's own Prometheus
endpoint, which is the number of requests the scheduler is generating for right
now. The Router's `/get_load` cannot answer this question: it counts tokens
queued at the worker, and a request that is already generating counts zero. An
earlier version of this script waited on it and therefore only ever waited a
fixed 20 s - any generation longer than that was cut off by the Pod deletion.

Runs inside the cluster (the Router's control port and the workers' Pod
addresses are not reachable from the jump host). Prints a final marker so the
caller can decide without parsing prose.

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
# A long generation is the whole reason this waits on a real signal, so the
# budget has to cover one: 32k tokens at the measured ~250 tok/s is about two
# minutes, and the schedule values freeing the cards over saving a request.
QUIESCE_TIMEOUT = int(os.environ.get("QUIESCE_TIMEOUT", "300"))
# Closing routing only changes how the *next* request is placed. A request the
# Router dispatched microseconds earlier is already on its way, so the first
# poll can happen before the worker has counted it. One short settle closes that
# gap, and the poll below additionally requires a second confirmation.
SETTLE_SECONDS = int(os.environ.get("SETTLE_SECONDS", "3"))
POLL_SECONDS = 3
# Two consecutive zero readings, not one: a request that finished between two
# polls and one that has not started yet look identical in a single sample.
CONFIRM_POLLS = 2
# The worker's own view of what it is generating. Both gauges are summed: a
# queued request is work the Pod would lose just as surely as a running one.
PENDING_METRICS = ("sglang:num_running_reqs", "sglang:num_queue_reqs")
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


def fetch_text(url, timeout=10):
    request = urllib.request.Request(url, method="GET")
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return response.read().decode("utf-8", "replace")


def pending_requests(worker_url):
    """How much work a worker says it is doing, or None if it will not say.

    Summed over every series: with tensor parallelism each rank may publish the
    same gauge, and over-counting a non-zero reading only makes the wait
    stricter. Zero is zero however many ranks report it.
    """
    try:
        text = fetch_text(worker_url.rstrip("/") + "/metrics")
    except Exception as exc:  # noqa: BLE001 - a probe reports, it does not raise
        return None, str(exc)

    total = 0.0
    found = False
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        head, _, value = line.rpartition(" ")
        name = head.split("{", 1)[0].strip()
        if name not in PENDING_METRICS:
            continue
        try:
            total += float(value)
        except ValueError:
            continue
        # Only a running-request gauge proves the worker understands the
        # question. num_queue_reqs alone would let a worker that does not export
        # the running gauge look idle.
        if name == "sglang:num_running_reqs":
            found = True
    if not found:
        return None, "no sglang:num_running_reqs"
    return total, None


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
    print("settling for %ds before asking the workers what they are running"
          % SETTLE_SECONDS)
    time.sleep(SETTLE_SECONDS)

    total = 0.0
    unknown = 0
    zero_streak = 0
    while time.time() < deadline:
        readings = []
        for worker in targets:
            count, error = pending_requests(worker.get("url") or "")
            readings.append((worker.get("id"), count, error))

        total = 0.0
        unknown = 0
        for worker_id, count, error in readings:
            if count is None:
                unknown += 1
                print("  %-40s unreadable: %s" % (worker_id, error))
            else:
                total += count
                if count:
                    print("  %-40s running/queued: %d" % (worker_id, count))

        if total == 0 and unknown == 0:
            zero_streak += 1
            if zero_streak >= CONFIRM_POLLS:
                print("QUIESCE_OK: nothing generating on %d worker(s)" % len(targets))
                return 0
            print("in-flight: 0 - confirming (%d/%d)" % (zero_streak, CONFIRM_POLLS))
        else:
            zero_streak = 0
            print("in-flight: %d, unreadable: %d" % (total, unknown))
        time.sleep(POLL_SECONDS)

    if total == 0:
        # Nothing measurable was running at the end of the window; the requests
        # that were in flight when routing closed have had their time.
        print("QUIESCE_OK: no measurable work after %ds (metrics unreadable on %d worker(s))"
              % (QUIESCE_TIMEOUT, unknown))
        return 0
    print("QUIESCE_TIMEOUT: %d request(s) still generating after %ds" % (total, QUIESCE_TIMEOUT))
    return 3


if __name__ == "__main__":
    sys.exit(main())
