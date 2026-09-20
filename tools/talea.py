#!/usr/bin/env python3
"""Talea operator and partner CLI. Requires only Python's standard library."""
import argparse
import datetime
import hashlib
import hmac
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path


def send(config, path, method="GET", payload=None, partner=False):
    body = json.dumps(payload, ensure_ascii=False).encode() if payload is not None else None
    token = config.get("push_token" if partner else "admin_token", "")
    if not token or (partner and not config.get("push_secret")):
        raise ValueError("客户端配置缺少所需凭据")
    headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json"}
    if partner:
        stamp = str(int(time.time()))
        headers["X-Capacity-Timestamp"] = stamp
        headers["X-Capacity-Signature"] = hmac.new(
            config["push_secret"].encode(), stamp.encode() + b"." + body, hashlib.sha256).hexdigest()
    request = urllib.request.Request(config["url"].rstrip("/") + path,
                                     data=body, headers=headers, method=method)
    # Never forward credentials to a redirect destination.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, hdrs, newurl):
            return None
    try:
        with urllib.request.build_opener(NoRedirect).open(request, timeout=60) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        raise RuntimeError("HTTP %s: %s" % (error.code, error.read(8192).decode(errors="replace"))) from None


def node_status(node):
    """Describe operational state; legacy instance IDLE only means prepared."""
    instance, service = node.get("instance_state"), node.get("service_state")
    if instance == "RELEASED":
        return "已回收"
    if node.get("pending_release"):
        if node.get("last_error"):
            return "回收重试中"
        if service == "DRAINING":
            return "摘流中"
        return "释放中" if instance == "RELEASING" else "等待回收"
    if instance == "LOST":
        return "节点失联"
    if instance == "RELEASING":
        return "摘流中" if service == "DRAINING" else "释放中"
    if service == "NONE":
        return {"ALLOCATING": "接入中", "PREPARING": "准备中", "IDLE": "空闲"}.get(instance, "未知状态")
    return {"SERVING": "服务中", "STARTING": "启动中", "HEALTHY": "待注册",
            "REGISTERING": "注册中", "DRAINING": "摘流中", "FAILED": "异常"}.get(service, "未知状态")


def main(argv=None):
    parser = argparse.ArgumentParser(description="Talea 节点管理与合作商 Push 客户端")
    parser.add_argument("--config", default=os.environ.get("TALEA_CLIENT_CONFIG", "/etc/tai-talea/client.json"))
    commands = parser.add_subparsers(dest="command", required=True)
    benchmark = commands.add_parser("benchmark", help="在控制节点运行 GSM8K 吞吐量测试", add_help=False)
    benchmark.add_argument("benchmark_args", nargs=argparse.REMAINDER)
    nodes = commands.add_parser("nodes", help="列出节点及实际运行状态")
    nodes.add_argument("--raw", action="store_true", help="附带原始容器生命周期和服务状态；IDLE 表示容器已就绪")
    commands.add_parser("login-token", help="显示用于网页登录的管理员令牌（仅本机管理员使用）")
    status = commands.add_parser("status", help="查看单个节点")
    status.add_argument("id")
    reclaim = commands.add_parser("reclaim", help="请求 Talea 自动摘流并回收节点")
    reclaim.add_argument("id")
    reclaim.add_argument("--grace", type=int, default=None, help="最长摘流等待秒数；默认采用服务端策略")
    push = commands.add_parser("push", help="合作商容量通知，自动生成签名")
    actions = push.add_subparsers(dest="action", required=True)
    add = actions.add_parser("add", help="上报新容量")
    add.add_argument("--file", required=True, help="容量对象 JSON 文件")
    revoke = actions.add_parser("revoke", help="通知容量撤销")
    revoke.add_argument("id")
    revoke.add_argument("--grace", type=int, default=60)
    for command in (add, revoke):
        command.add_argument("--event-id", help="同一业务事件重试时复用此 ID")
    # Delegate benchmark options to its own parser, preserving --help.
    arguments = list(sys.argv[1:] if argv is None else argv)
    if "benchmark" in arguments:
        position = arguments.index("benchmark")
        args = parser.parse_args(arguments[:position+1])
        import talea_benchmark
        cfg = json.loads(Path(args.config).read_text(encoding="utf-8-sig"))
        return talea_benchmark.main(arguments[position+1:], cfg)
    args = parser.parse_args(arguments)
    cfg = json.loads(Path(args.config).read_text(encoding="utf-8-sig"))
    if args.command == "login-token":
        value = cfg.get("console_token") or cfg.get("admin_token")
        if not value:
            raise ValueError("此配置没有网页登录令牌")
        print(value)
        return 0
    if getattr(args, "grace", None) is not None and not 0 <= args.grace <= 86400:
        parser.error("grace 必须在 0 到 86400 秒之间")
    if args.command == "nodes":
        data = send(cfg, "/v1/capacity/instances")
        headers = ["节点", "角色", "节点状态"]
        if args.raw:
            headers += ["容器生命周期(原始)", "服务状态(原始)"]
        print("\t".join(headers + ["回收中"]))
        for node in data.get("instances", []):
            values = [node["id"], node.get("role") or "-", node_status(node)]
            if args.raw:
                values += [node["instance_state"], node["service_state"]]
            print("\t".join(values + ["是" if node.get("pending_release") else "否"]))
        return 0
    if args.command in ("status", "reclaim"):
        path = "/v1/capacity/instances/" + urllib.parse.quote(args.id, safe="")
        if args.command == "reclaim":
            payload = {} if args.grace is None else {"grace_seconds": args.grace}
            data = send(cfg, path + "/reclaim", "POST", payload)
        else:
            data = send(cfg, path)
    else:
        partner_id = cfg.get("partner_id")
        if not partner_id:
            raise ValueError("合作商配置缺少 partner_id")
        event_id = args.event_id or str(uuid.uuid4())
        event = {"event_id": event_id, "partner_id": partner_id,
                 "type": "CAPACITY_ADDED" if args.action == "add" else "CAPACITY_REVOKED",
                 "occurred_at": datetime.datetime.now(datetime.timezone.utc).isoformat()}
        if args.action == "add":
            event["instance"] = json.loads(Path(args.file).read_text(encoding="utf-8-sig"))
        else:
            event.update(instance={"id": args.id}, grace_seconds=args.grace)
        print("event_id=" + event_id + "（重试同一事件时使用 --event-id）", file=sys.stderr)
        data = send(cfg, "/v1/capacity/events", "POST", event, partner=True)
        if not data.get("accepted") or data.get("result") in ("FAILED", "REJECTED"):
            raise RuntimeError(json.dumps(data, ensure_ascii=False))
    print(json.dumps(data, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, RuntimeError, urllib.error.URLError) as error:
        print("talea: " + str(error), file=sys.stderr)
        sys.exit(1)
