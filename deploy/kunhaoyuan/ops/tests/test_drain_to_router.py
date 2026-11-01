"""The drain wait must end when the work ends, not when the clock says so.

Every case here exists because of a real failure: the script used to wait on
`sglang:num_running_reqs`, a gauge this sglang build does not export, so no
reading was ever readable, no zero was ever confirmed, and every drain burned
its full QUIESCE_TIMEOUT. That is indistinguishable from sleeping, and nothing
in the deployment noticed for weeks.

The stub stands in for the Router's control port. `POLL_SECONDS` is patched down
so the suite runs in seconds; everything else goes through the real code path,
including how `load` is interpreted and which exit code comes out.

Run directly or under pytest:
    python3 deploy/kunhaoyuan/ops/tests/test_drain_to_router.py
"""

import importlib.util
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SCRIPT = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                      "drain-to-router.py")

LOADED = {"n": 0}


def load_script(router_url, timeout=60, mode="close", settle=0):
    """Fresh module each time: BASE and the budgets are module-level constants."""
    os.environ["ROUTER_URL"] = router_url
    os.environ["QUIESCE_TIMEOUT"] = str(timeout)
    os.environ["MODE"] = mode
    os.environ["SETTLE_SECONDS"] = str(settle)

    LOADED["n"] += 1
    spec = importlib.util.spec_from_file_location("drain_under_test_%d" % LOADED["n"], SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    module.POLL_SECONDS = 0.2  # the only thing shortened; the logic is unchanged
    return module


class Stub:
    def __init__(self):
        self.loads = {}
        self.workers_ok = True
        # When set, only that many /workers reads succeed and later ones fail.
        self.ok_reads_left = None
        self.puts = []
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), self._handler())
        self.url = "http://127.0.0.1:%d" % self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()

    def readable(self):
        if not self.workers_ok:
            return False
        if self.ok_reads_left is None:
            return True
        if self.ok_reads_left <= 0:
            return False
        self.ok_reads_left -= 1
        return True

    def _handler(stub_self):
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def _send(self, code, payload):
                body = json.dumps(payload).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self):
                if self.path != "/workers":
                    return self._send(404, {"error": "not found"})
                if not stub_self.readable():
                    return self._send(503, {"error": "unavailable"})
                workers = [
                    {
                        "id": worker_id,
                        "url": "http://10.222.0.%d:9002" % (index + 1),
                        "worker_type": "regular",
                        "is_healthy": True,
                        "load": load,
                    }
                    for index, (worker_id, load) in enumerate(stub_self.loads.items())
                ]
                self._send(200, {"workers": workers, "total": len(workers)})

            def do_PUT(self):
                length = int(self.headers.get("Content-Length") or 0)
                self.rfile.read(length)
                stub_self.puts.append(self.path)
                self._send(200, {"ok": True})

        return Handler


def timed(module):
    began = time.time()
    rc = module.main()
    return rc, time.time() - began


def test_an_idle_pool_confirms_without_waiting_out_the_window():
    stub = Stub()
    try:
        stub.loads = {"w1": 0, "w2": 0}
        rc, took = timed(load_script(stub.url, timeout=60))
        assert rc == 0
        # Two confirmations at 0.2 s plus overhead: if this ever takes the
        # timeout, the wait has stopped reading something real again.
        assert took < 5, "idle pool took %.1fs" % took
    finally:
        stub.close()


def test_it_waits_while_a_worker_reports_work_and_leaves_when_it_clears():
    stub = Stub()
    try:
        stub.loads = {"w1": 3, "w2": 0}

        def clear():
            time.sleep(0.8)
            stub.loads = {"w1": 0, "w2": 0}

        threading.Thread(target=clear, daemon=True).start()
        rc, took = timed(load_script(stub.url, timeout=60))
        assert rc == 0
        assert took >= 0.8, "returned before the work cleared: %.1fs" % took
        assert took < 5, "did not notice the work had cleared: %.1fs" % took
    finally:
        stub.close()


def test_work_that_never_clears_is_reported_as_failure():
    stub = Stub()
    try:
        stub.loads = {"w1": 1, "w2": 0}
        rc, _ = timed(load_script(stub.url, timeout=1))
        assert rc == 3, "a window that expires with work left is not success"
    finally:
        stub.close()


def test_a_router_that_will_not_report_is_not_assumed_idle():
    stub = Stub()
    try:
        stub.loads = {"w1": 0, "w2": 0}
        # The first read succeeds (so the close step happens), every poll after
        # it fails: the Router went away while we were waiting on it.
        stub.ok_reads_left = 1
        rc, _ = timed(load_script(stub.url, timeout=1))
        assert rc == 3, "an unreadable Router must not be read as 'nothing running'"
    finally:
        stub.close()


def test_open_mode_does_not_wait():
    stub = Stub()
    try:
        stub.loads = {"w1": 5, "w2": 0}
        rc, took = timed(load_script(stub.url, timeout=60, mode="open"))
        assert rc == 0
        assert took < 2, "open should not wait for in-flight: %.1fs" % took
        assert sorted(stub.puts) == ["/state/workers/w1", "/state/workers/w2"]
    finally:
        stub.close()


if __name__ == "__main__":
    failures = 0
    for name, case in sorted(globals().items()):
        if not name.startswith("test_") or not callable(case):
            continue
        try:
            case()
            print("ok   %s" % name)
        except AssertionError as exc:
            failures += 1
            print("FAIL %s: %s" % (name, exc))
    print("%d failure(s)" % failures)
    sys.exit(1 if failures else 0)
