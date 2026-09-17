#!/usr/bin/env python3
"""Read-only probe client for Alaya NeW Cloud (九章智算云) CCI.

Standard library only, matching the project's "no dependencies" convention.

Credentials come from the environment and are never written to disk or echoed:

    export ALAYANEW_AK=ak_xxx
    export ALAYANEW_SK=sk_xxx

Usage:

    python cci_probe.py doctor                 # 探测 API 前缀与认证是否可用
    python cci_probe.py list [--status RUNNING]
    python cci_probe.py detail <instance-id>
    python cci_probe.py ssh <instance-id>
    python cci_probe.py ports <instance-id>
    python cci_probe.py raw GET /api/osm/v1/cci/instance/list pageNo=1 pageSize=5

Signing (per the official auth reference):

    StringToSign = Timestamp + "|" + HTTPMethod + "|" + URI
    signature    = HexEncode(HMAC-SHA256(sk, StringToSign))
    Authorization: alayanew-HMAC-SHA256 {ak}:{timestamp}:{signature}

The timestamp is Unix milliseconds and the server accepts a +/- 5 minute window.
URI carries the context path but no scheme, host, port or query string.

Reference: docs/alayanew-cci.md in this repository.
"""

from __future__ import annotations

import argparse
import hashlib
import hmac
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE_URL = "https://api.alayanew.com"

# The documented cURL examples omit this prefix and return the web app instead of
# JSON; /api/osm is what the authentication reference itself uses. `doctor`
# verifies this against the live service.
DEFAULT_PREFIX = "/api/osm"
ALT_PREFIX = "/api/v1"

USER_AGENT = "tai-talea-cci-probe/1"

# Some workstations export HTTP_PROXY for a local tunnel that is not always
# running, which turns every API call into "connection refused" even though the
# service is reachable directly. --no-proxy builds an opener with an empty proxy
# map so the request goes straight out.
_NO_PROXY = False


def _opener():
    if _NO_PROXY:
        return urllib.request.build_opener(urllib.request.ProxyHandler({}))
    return urllib.request.build_opener()


class Credentials:
    """AccessKey pair, kept out of every log line."""

    def __init__(self, ak: str, sk: str) -> None:
        if not ak or not sk:
            raise SystemExit(
                "missing credentials: set ALAYANEW_AK and ALAYANEW_SK "
                "(console -> 客户中心/权限管理/访问管理 -> 创建 AccessKey)"
            )
        self.ak = ak
        self.sk = sk

    @classmethod
    def from_env(cls) -> "Credentials":
        return cls(
            os.environ.get("ALAYANEW_AK", "").strip(),
            os.environ.get("ALAYANEW_SK", "").strip(),
        )

    def __repr__(self) -> str:  # pragma: no cover - defensive
        return "Credentials(ak=%s..., sk=<hidden>)" % self.ak[:6]


def sign(credentials: Credentials, method: str, uri: str, timestamp_ms: int | None = None) -> str:
    """Build the Authorization header value.

    `uri` must be the path shown in the request line, i.e. it includes the
    context path but excludes the query string.
    """
    if timestamp_ms is None:
        timestamp_ms = int(time.time() * 1000)
    string_to_sign = "%d|%s|%s" % (timestamp_ms, method.upper(), uri)
    signature = hmac.new(
        credentials.sk.encode("utf-8"), string_to_sign.encode("utf-8"), hashlib.sha256
    ).hexdigest()
    return "alayanew-HMAC-SHA256 %s:%d:%s" % (credentials.ak, timestamp_ms, signature)


class ApiError(RuntimeError):
    def __init__(self, status: int, payload: object) -> None:
        super().__init__("HTTP %s: %s" % (status, payload))
        self.status = status
        self.payload = payload

    @property
    def business_status(self) -> int | None:
        if isinstance(self.payload, dict):
            value = self.payload.get("status")
            if isinstance(value, int):
                return value
        return None


def call(
    credentials: Credentials,
    method: str,
    path: str,
    query: dict | None = None,
    body: object | None = None,
    *,
    prefix: str = DEFAULT_PREFIX,
    timeout: float = 30.0,
) -> object:
    """Perform one signed request and return the decoded JSON payload."""
    uri = prefix + path
    url = BASE_URL + uri
    if query:
        url += "?" + urllib.parse.urlencode(query)

    data = None
    headers = {
        "accept": "application/json",
        "Authorization": sign(credentials, method, uri),
        "User-Agent": USER_AGENT,
    }
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"

    request = urllib.request.Request(url, data=data, headers=headers, method=method.upper())
    try:
        with _opener().open(request, timeout=timeout) as response:
            raw = response.read().decode("utf-8", errors="replace")
            return json.loads(raw) if raw.strip() else {}
    except urllib.error.HTTPError as error:
        raw = error.read().decode("utf-8", errors="replace")
        try:
            payload = json.loads(raw)
        except json.JSONDecodeError:
            payload = raw[:500]
        raise ApiError(error.code, payload) from None


def unwrap(payload: object) -> object:
    """Return `data` from the platform envelope {"status":..,"message":..,"data":..}."""
    if isinstance(payload, dict):
        status = payload.get("status")
        if status is not None and status != 200:
            raise SystemExit("platform error: status=%s message=%s" % (status, payload.get("message")))
        if "data" in payload:
            return payload["data"]
    return payload


# Business codes the platform returns when the signature itself is the problem.
# They are reported separately from an unknown prefix: both prefixes answer with
# the same auth error, so a bad AccessKey looks exactly like a bad path here.
AUTH_ERROR_CODES = {
    40001: "缺少 Authorization 头或格式错误",
    40002: "时间戳格式错误或超出 ±5 分钟窗口",
    40003: "签名验证失败",
    40004: "Access Key 已禁用",
    40005: "Access Key 已过期",
    40006: "Access Key 不存在",
    40401: "未携带凭据（网关层拒绝）",
}


def probe_prefix(credentials: Credentials) -> str:
    """Return whichever API prefix the service actually serves.

    A rejected credential short-circuits: every prefix answers identically in
    that case, so the prefix question cannot be settled until the AccessKey works.
    """
    uri_path = "/v1/cci/instance/list"
    query = {"pageNo": 1, "pageSize": 1}
    results = []
    auth_failures = []
    for prefix in (DEFAULT_PREFIX, ALT_PREFIX):
        try:
            payload = call(credentials, "GET", uri_path, query, prefix=prefix)
        except ApiError as error:
            code = error.business_status
            if code in AUTH_ERROR_CODES:
                auth_failures.append((prefix, code))
                continue
            results.append((prefix, "HTTP %s %s" % (error.status, error.payload)))
            continue
        if isinstance(payload, dict) and payload.get("status") == 200:
            return prefix
        results.append((prefix, "unexpected payload: %s" % json.dumps(payload)[:200]))

    if auth_failures:
        code = auth_failures[0][1]
        raise SystemExit(
            "credentials rejected by the service, so the API prefix cannot be "
            "determined yet:\n"
            + "\n".join("  %-10s status=%s" % (prefix, c) for prefix, c in auth_failures)
            + "\n  status=%s -> %s" % (code, AUTH_ERROR_CODES.get(code, "unknown"))
            + "\n  check ALAYANEW_AK / ALAYANEW_SK; the signing scheme itself is "
            "accepted (the gateway parsed the header)."
        )
    raise SystemExit(
        "no usable API prefix found:\n"
        + "\n".join("  %-10s %s" % (prefix, detail) for prefix, detail in results)
    )


def redact_ssh(payload: object, *, show_password: bool) -> dict:
    """Keep the SSH password out of the terminal unless explicitly requested."""
    if not isinstance(payload, dict):
        return {"raw": payload}
    result = dict(payload)
    if not show_password:
        for key in list(result):
            if "密码" in key or key.lower() in ("password", "passwd"):
                result[key] = "<hidden, pass --show-password to reveal>"
    return result


# --------------------------------------------------------------------------- commands


def cmd_doctor(credentials: Credentials, args: argparse.Namespace) -> int:
    print("API base      :", BASE_URL)
    try:
        prefix = probe_prefix(credentials)
    except SystemExit as failure:
        print("prefix        : UNKNOWN")
        print(failure)
        return 1
    print("API prefix    :", prefix, "(verified against the live service)")

    payload = unwrap(call(credentials, "GET", "/v1/cci/instance/list",
                          {"pageNo": 1, "pageSize": 1}, prefix=prefix))
    records = payload.get("records", []) if isinstance(payload, dict) else []
    print("auth          : OK")
    print("instances     : totalRows =", payload.get("totalRows") if isinstance(payload, dict) else "?")
    for record in records:
        print("   first      : id=%s name=%s status=%s" % (
            record.get("id"), record.get("name"), record.get("status")))
    return 0


def cmd_list(credentials: Credentials, args: argparse.Namespace) -> int:
    query: dict = {"pageNo": args.page, "pageSize": args.size}
    if args.status:
        query["status"] = args.status
    if args.name:
        query["name"] = args.name
    payload = unwrap(call(credentials, "GET", "/v1/cci/instance/list", query))
    records = payload.get("records", []) if isinstance(payload, dict) else []
    print("totalRows=%s  page=%s  size=%s" % (
        payload.get("totalRows") if isinstance(payload, dict) else "?",
        payload.get("pageNo") if isinstance(payload, dict) else "?",
        payload.get("pageSize") if isinstance(payload, dict) else "?",
    ))
    for record in records:
        resource = record.get("resource") or {}
        print("- %s" % record.get("id"))
        print("    name      %s" % record.get("name"))
        print("    status    %s / %s" % (record.get("status"), record.get("subStatus")))
        print("    gpu       %s x%s   cpu=%s mem=%sGB disk=%sGB" % (
            resource.get("gpuName"), resource.get("gpuCount"),
            resource.get("cpuCores"), resource.get("memoryGB"), resource.get("diskGB")))
        print("    image     %s" % record.get("image"))
        print("    aidcId    %s   startTime=%s" % (record.get("aidcId"), record.get("startTime")))
        print("    access    webSSH=%s jupyter=%s vscode=%s" % (
            bool(record.get("webSSHUrl")), bool(record.get("jupyterUrl")), bool(record.get("vscodeUrl"))))
    return 0


def cmd_detail(credentials: Credentials, args: argparse.Namespace) -> int:
    payload = unwrap(call(credentials, "GET", "/v1/cci/instance/%s" % args.instance_id))
    print(json.dumps(payload, indent=2, ensure_ascii=False))
    return 0


def cmd_ssh(credentials: Credentials, args: argparse.Namespace) -> int:
    payload = unwrap(call(credentials, "POST", "/v1/cci/instance/%s/get-ssh" % args.instance_id))
    print(json.dumps(redact_ssh(payload, show_password=args.show_password),
                     indent=2, ensure_ascii=False))
    return 0


def cmd_ports(credentials: Credentials, args: argparse.Namespace) -> int:
    """Show the port mappings, which is how a container port becomes reachable.

    Response shape: data.ports[] holds the system ports (9001/9002, exposed with
    no protocol attached) and data.customPorts[] holds what was opened by hand.
    """
    payload = unwrap(call(credentials, "GET",
                          "/v1/cci/instance/%s/open-port/list" % args.instance_id))
    if not isinstance(payload, dict):
        print(json.dumps(payload, indent=2, ensure_ascii=False))
        return 0
    for group in ("ports", "customPorts"):
        entries = payload.get(group) or []
        print("%s (%d):" % (group, len(entries)))
        for entry in entries:
            host = entry.get("externalHost") or "?"
            external = entry.get("externalPort")
            internal = entry.get("port")
            print("   %-22s -> %s:%s   (internal %s)" % (
                entry.get("protocol") or "tcp", host, external, internal))
        if not entries:
            print("   (none)")
    return 0


def cmd_raw(credentials: Credentials, args: argparse.Namespace) -> int:
    query = {}
    for pair in args.params:
        if "=" not in pair:
            raise SystemExit("parameters must be key=value, got %r" % pair)
        key, value = pair.split("=", 1)
        query[key] = value
    payload = call(credentials, args.method, args.path, query or None,
                   body=None, prefix=args.prefix)
    print(json.dumps(payload, indent=2, ensure_ascii=False))
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--prefix", default=DEFAULT_PREFIX,
                        help="API path prefix (default: %(default)s)")
    parser.add_argument("--no-proxy", action="store_true",
                        help="ignore HTTP_PROXY/HTTPS_PROXY and connect directly")
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("doctor", help="verify the API prefix and credentials").set_defaults(func=cmd_doctor)

    listing = sub.add_parser("list", help="list CCI instances")
    listing.add_argument("--status", choices=["PENDING", "RUNNING", "STOPPED", "ERROR", "RELEASING"])
    listing.add_argument("--name")
    listing.add_argument("--page", type=int, default=1)
    listing.add_argument("--size", type=int, default=20)
    listing.set_defaults(func=cmd_list)

    detail = sub.add_parser("detail", help="show one instance")
    detail.add_argument("instance_id")
    detail.set_defaults(func=cmd_detail)

    ssh = sub.add_parser("ssh", help="show SSH connection info")
    ssh.add_argument("instance_id")
    ssh.add_argument("--show-password", action="store_true")
    ssh.set_defaults(func=cmd_ssh)

    ports = sub.add_parser("ports", help="show the open port mappings")
    ports.add_argument("instance_id")
    ports.set_defaults(func=cmd_ports)

    raw = sub.add_parser("raw", help="issue an arbitrary signed request")
    raw.add_argument("method")
    raw.add_argument("path")
    raw.add_argument("params", nargs="*", help="query parameters as key=value")
    raw.set_defaults(func=cmd_raw)

    return parser


def main(argv: list[str] | None = None) -> int:
    global _NO_PROXY
    args = build_parser().parse_args(argv)
    _NO_PROXY = bool(getattr(args, "no_proxy", False))
    credentials = Credentials.from_env()
    try:
        return args.func(credentials, args)
    except ApiError as error:
        business = error.business_status
        hint = AUTH_ERROR_CODES.get(business, "")
        if hint:
            hint = "\n  -> " + hint
        print("API error: HTTP %s business=%s\n  payload: %s%s" % (
            error.status, business, error.payload, hint), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
