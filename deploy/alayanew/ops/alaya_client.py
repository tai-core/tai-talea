#!/usr/bin/env python3
"""Alaya NeW (九章智算云) OpenAPI client - HMAC-SHA256 auth.

StringToSign = "{ts_ms}|{METHOD}|{uri}"   (uri = path, no query, no host)
Authorization: alayanew-HMAC-SHA256 {ak}:{ts}:{sig_hex_lower}

Credentials come from the environment on purpose: this repository is public and
the AK/SK belong to the tenant, not to the code.

    export TALEA_ALAYA_AK=ak_...
    export TALEA_ALAYA_SK=sk_...

Usage:
    from alaya_client import call
    code, body = call("GET", "/api/osm/v1/cci/instance/list", "pageNo=1&pageSize=50")
"""
import hashlib
import hmac
import json
import os
import sys
import time
import urllib.error
import urllib.request

AK = os.environ.get("TALEA_ALAYA_AK", "")
SK = os.environ.get("TALEA_ALAYA_SK", "")
BASE = os.environ.get("TALEA_ALAYA_BASE", "https://api.alayanew.com")

# TALEA_ALAYA_DIRECT=1 bypasses ALL proxies: on Windows urllib falls back to
# the registry proxy once the env vars are unset, and local proxies that
# cannot tunnel to the platform produce either "Tunnel connection failed"
# or a getaddrinfo failure on the proxy host itself.
_DIRECT = os.environ.get("TALEA_ALAYA_DIRECT", "") == "1"
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({})) if _DIRECT \
    else urllib.request.build_opener()


def sign(method, uri):
    """Build the Authorization header for one request."""
    if not AK or not SK:
        raise SystemExit(
            "TALEA_ALAYA_AK / TALEA_ALAYA_SK are not set; see deploy/alayanew/README.md"
        )
    ts = int(time.time() * 1000)
    sts = f"{ts}|{method.upper()}|{uri}"
    sig = hmac.new(SK.encode(), sts.encode(), hashlib.sha256).hexdigest()
    return f"alayanew-HMAC-SHA256 {AK}:{ts}:{sig}"


def call(method, path, query="", body=None, extra_headers=None, timeout=30):
    """One API call. Returns (http_status, parsed_json); -1 on transport error."""
    url = BASE + path + (("?" + query) if query else "")
    headers = {
        "accept": "application/json",
        "Authorization": sign(method, path),
    }
    if extra_headers:
        headers.update(extra_headers)
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method.upper())
    try:
        with _OPENER.open(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read().decode())
        except Exception:
            return e.code, {"raw": "<non-json>"}
    except Exception as e:
        return -1, {"error": str(e)}


def main():
    """Tiny CLI so the module can be used from a shell too.

    POST/PUT/PATCH read a JSON body from stdin, which is how the port calls are
    made (``echo '{"port":8787}' | alaya_client.py POST .../close-port``).
    """
    if len(sys.argv) < 3:
        print(__doc__)
        print("usage: alaya_client.py METHOD PATH [QUERY]   # body on stdin for POST")
        return
    method, path = sys.argv[1], sys.argv[2]
    query = sys.argv[3] if len(sys.argv) > 3 else ""
    body = None
    if method.upper() in ("POST", "PUT", "PATCH") and not sys.stdin.isatty():
        text = sys.stdin.read().strip()
        body = json.loads(text) if text else None
    code, reply = call(method, path, query, body=body)
    print(code, json.dumps(reply, ensure_ascii=False)[:2000])


if __name__ == "__main__":
    main()
