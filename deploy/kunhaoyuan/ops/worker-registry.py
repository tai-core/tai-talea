#!/usr/bin/env python3
"""Report whether the Router will route to the workers that exist right now.

Two things make "the deployment is up" hard to answer from outside:

  * A worker Pod reports Ready as soon as its bootstrap answers
    /bootstrap/health, and that endpoint answers 200 while the model is still
    loading. `kubectl get pods` therefore cannot tell a warm deployment from a
    cold one.
  * An inference probe only proves that *one* worker is warm. The Router
    round-robins, so a single 200 says nothing about the other three.

The Router's registry is the component that actually decides, but two of its
answers are not live: `is_healthy` is a cached health-check flag, and a worker
that disappears never unregisters, so its row keeps the last flag it had. A
count of healthy rows can therefore be right about a generation of Pods that is
already gone.

So the question this asks is narrower and honest: for each Pod that exists now,
is its service URL both registered as healthy and actually answering? That is
the only state in which traffic will reach it.

env:
  EXPECTED  comma separated service URLs of the Pods that should be serving,
            e.g. http://10.222.2.211:9002,http://10.222.3.36:9002
  ROUTER_URL  control port of the Router (default http://talea-router:30001)

usage: worker-registry.py [--wait] [--timeout S] [--interval S]

Without --wait it prints one reading and exits 0 if the registry was readable.
With --wait it polls until every expected URL is serving, and exits 1 on
timing out.
"""
import argparse
import json
import os
import sys
import time
import urllib.parse
import urllib.request

ROUTER = os.environ.get("ROUTER_URL", "http://talea-router:30001")
EXPECTED = [u.strip() for u in os.environ.get("EXPECTED", "").split(",") if u.strip()]


def read_registry(timeout=10):
    with urllib.request.urlopen(ROUTER + "/workers", timeout=timeout) as resp:
        body = json.loads(resp.read().decode())
    rows = body if isinstance(body, list) else (body.get("workers") or body.get("data") or [])
    return rows


def answers(url, timeout=5):
    """Whether the worker itself says it is up, bypassing the Router's cache."""
    try:
        with urllib.request.urlopen(url.rstrip("/") + "/health", timeout=timeout) as resp:
            return resp.status == 200
    except Exception:
        return False


def sample():
    rows = read_registry()
    by_url = {}
    for row in rows:
        url = str(row.get("url") or "").rstrip("/")
        # Two registrations can share a URL only if the Router reused the id,
        # but keep the first so the table has one line per address.
        by_url.setdefault(url, bool(row.get("is_healthy")))
    return rows, by_url


def render(rows, by_url):
    healthy = [u for u, ok in by_url.items() if ok]
    print("registered=%d router_healthy=%d expected=%d"
          % (len(by_url), len(healthy), len(EXPECTED)), flush=True)
    seen = set()
    for url in EXPECTED or sorted(by_url, key=lambda u: (not by_url[u], u)):
        seen.add(url)
        flag = "ready" if by_url.get(url) else ("stale" if url in by_url else "missing")
        live = "live" if answers(url) else "dead"
        print("  %-8s %-26s %s" % (flag, url, live), flush=True)
    for url in sorted(by_url):
        if url in seen:
            continue
        print("  %-8s %-26s %s" % (
            "ready" if by_url[url] else "stale", url,
            "live" if answers(url) else "dead",
        ), flush=True)


def serving(by_url):
    return [u for u in EXPECTED if by_url.get(u) and answers(u)]


def prune(by_url):
    """Drop registrations that describe nothing that exists any more.

    The registry is keyed by the URL a worker was registered with, and a worker
    that disappears never unregisters, so every generation leaves its rows
    behind. They are unhealthy, so they are never routed to, but the health
    checker keeps probing addresses that no longer exist and the table stops
    being readable. Deleting needs both conditions: not expected (so a live Pod
    is never touched) and not answering /health (so a recycled address that a
    new worker already claimed is never touched either).
    """
    expect = set(EXPECTED)
    removed, failed = [], []
    for url in sorted(by_url):
        if url in expect or answers(url):
            continue
        try:
            req = urllib.request.Request(
                ROUTER + "/workers/by-url?" + urllib.parse.urlencode({"url": url}),
                method="DELETE",
            )
            with urllib.request.urlopen(req, timeout=10) as resp:
                ok = resp.status in (200, 202, 204)
        except Exception as exc:
            failed.append("%s: %s" % (url, exc))
            continue
        if ok:
            removed.append(url)
            print("  removed %s" % url, flush=True)
        else:
            failed.append(url)
    print("pruned=%d kept=%d failed=%d" % (len(removed), len(by_url) - len(removed), len(failed)),
          flush=True)
    for item in failed:
        print("  failed %s" % item, flush=True)
    return 0 if not failed else 1


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--wait", action="store_true")
    parser.add_argument("--prune", action="store_true")
    parser.add_argument("--timeout", type=int, default=600)
    parser.add_argument("--interval", type=int, default=15)
    args = parser.parse_args()

    if args.prune:
        try:
            _, by_url = sample()
        except Exception as exc:
            print("registry unreadable: %s: %s" % (type(exc).__name__, exc), flush=True)
            return 1
        return prune(by_url)

    deadline = time.time() + args.timeout
    while True:
        try:
            rows, by_url = sample()
        except Exception as exc:
            # An unreadable registry is not a count of zero; name the failure.
            print("registry unreadable: %s: %s" % (type(exc).__name__, exc), flush=True)
            if not args.wait:
                return 1
            by_url = None
        else:
            if not EXPECTED:
                render(rows, by_url)
                return 0
            up = serving(by_url)
            if len(up) == len(EXPECTED):
                render(rows, by_url)
                return 0
            if not args.wait:
                render(rows, by_url)
                return 1
            print("serving=%d/%d, waiting" % (len(up), len(EXPECTED)), flush=True)
        if time.time() >= deadline or not args.wait:
            if by_url is not None:
                render(rows, by_url)
            return 1
        time.sleep(args.interval)


if __name__ == "__main__":
    sys.exit(main())
