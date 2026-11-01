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

The wait reads the Router's `load` field for each worker, from the same
`/workers` response the close step already used. That number is the count of
requests this Router has dispatched to the worker and not yet finished
delivering: the worker struct increments it before the request is sent and the
guard decrements it when the response body is dropped, so a stream counts until
its last byte. It is the only number in this system that answers "is this worker
still generating for someone".

It used to read the worker's `sglang:num_running_reqs` from its own /metrics
instead. That gauge does not exist in this sglang build (the endpoint exports
prefill/decode token counters, cuda graph passes, http_requests_total and
process stats), so every poll came back unreadable, no zero reading ever
confirmed, and the wait always burned the full QUIESCE_TIMEOUT. A wait that
cannot end early is not a wait, it is a fixed delay with a progress bar.

What the count does not include, and why that is acceptable:

  * requests that reached the worker through some other Router - there is one;
  * requests whose client disconnected, because the Router drops the guard with
    the stream while the worker may keep generating until it notices.

Both omissions make the number too low, never too high, so a zero reading is at
worst optimistic by a request that is already being torn down. The alternative
errors were in the other direction and cost a fixed five minutes every night.

Runs inside the cluster (the Router's control port is not reachable from the
jump host). Prints a final marker so the caller can decide without parsing
prose.

MODE=close (default) closes routing and waits for in-flight to finish.
MODE=open puts the workers back, which is the only way to undo a close: the
control plane reads the Router's readiness but never writes it back on its own,
so a hand-drained pool would stay unroutable until the Pods were recreated.

exit 0  closed and quiesced, or reopened
exit 2  at least one worker refused the change - the pool is half changed
exit 3  requests still in flight when the quiesce window expired
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
# Router is in the middle of placing has been accepted but has not yet produced
# a load increment, so the first poll can happen before it is counted. One short
# settle closes that gap, and the poll below additionally requires a second
# confirmation.
SETTLE_SECONDS = int(os.environ.get("SETTLE_SECONDS", "3"))
POLL_SECONDS = 3
# Two consecutive zero readings, not one: a request that finished between two
# polls and one that has not started yet look identical in a single sample.
CONFIRM_POLLS = 2
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


def read_loads():
    """In-flight request count per worker id, or None if the Router will not say."""
    try:
        payload = call("GET", "/workers")
    except (urllib.error.URLError, OSError, ValueError) as exc:
        return None, str(exc)
    loads = {}
    for worker in payload.get("workers") or []:
        worker_id = worker.get("id")
        if worker_id is not None:
            loads[worker_id] = worker.get("load")
    return loads, None


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
    print("settling for %ds before asking the Router what is still in flight"
          % SETTLE_SECONDS)
    time.sleep(SETTLE_SECONDS)

    total = 0
    unreadable = 0
    zero_streak = 0
    while time.time() < deadline:
        loads, error = read_loads()
        if loads is None:
            # A Router that stopped answering tells us nothing about its workers,
            # and "nothing is running" is exactly the wrong thing to assume.
            total, unreadable = 0, len(targets)
            print("  the Router would not report loads: %s" % error)
        else:
            total, unreadable = 0, 0
            for worker in targets:
                worker_id = worker.get("id")
                if worker_id not in loads:
                    # Deregistered while we watched: it cannot be receiving
                    # anything, and there is no counter left to wait on.
                    print("  %-40s no longer registered" % worker_id)
                    continue
                load = loads[worker_id]
                if not isinstance(load, int) or isinstance(load, bool):
                    unreadable += 1
                    print("  %-40s no usable load field: %r" % (worker_id, load))
                    continue
                total += load
                if load:
                    print("  %-40s in flight: %d" % (worker_id, load))

        if total == 0 and unreadable == 0:
            zero_streak += 1
            if zero_streak >= CONFIRM_POLLS:
                print("QUIESCE_OK: nothing in flight on %d worker(s)" % len(targets))
                return 0
            print("in flight: 0 - confirming (%d/%d)" % (zero_streak, CONFIRM_POLLS))
        else:
            zero_streak = 0
            print("in flight: %d, unreadable: %d" % (total, unreadable))
        time.sleep(POLL_SECONDS)

    if total == 0 and unreadable == 0:
        # Nothing measurable was running at the end of the window; the requests
        # that were in flight when routing closed have had their time.
        print("QUIESCE_OK: nothing in flight after %ds" % QUIESCE_TIMEOUT)
        return 0
    print("QUIESCE_TIMEOUT: %d request(s) still in flight after %ds, %d worker(s) unreadable"
          % (total, QUIESCE_TIMEOUT, unreadable))
    return 3


if __name__ == "__main__":
    sys.exit(main())
