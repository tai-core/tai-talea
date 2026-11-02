#!/usr/bin/env python3
"""A pass-through proxy in front of the simulator that deletes total_tokens
from every usage object in the stream.

Why: sglang's /generate only carries a total when the upstream happened to
produce one, which is how L114 ended up with input/output but no total. The
simulator always fills total in, so it cannot reproduce that shape. This shim
does, and nothing else - every other byte is forwarded untouched.

usage: strip_total.py <listen_port> <upstream_port>
"""
import http.client
import http.server
import json
import sys

LISTEN = int(sys.argv[1]) if len(sys.argv) > 1 else 8090
UPSTREAM_PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 8080

HOP = {"host", "content-length", "connection", "transfer-encoding", "keep-alive"}


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _forward_headers(self):
        return {k: v for k, v in self.headers.items() if k.lower() not in HOP}

    def do_GET(self):
        conn = http.client.HTTPConnection("127.0.0.1", UPSTREAM_PORT, timeout=30)
        conn.request("GET", self.path, headers=self._forward_headers())
        res = conn.getresponse()
        body = res.read()
        self.send_response(res.status)
        for k, v in res.getheaders():
            if k.lower() not in HOP:
                self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        conn.close()

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length)
        conn = http.client.HTTPConnection("127.0.0.1", UPSTREAM_PORT, timeout=120)
        conn.request("POST", self.path, body=body, headers=self._forward_headers())
        res = conn.getresponse()

        self.send_response(res.status)
        self.send_header("Content-Type", res.getheader("Content-Type", "application/json"))
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        while True:
            line = res.readline()
            if not line:
                break
            out = line
            if line.startswith(b"data: ") and b'"usage"' in line:
                try:
                    payload = json.loads(line[6:])
                except ValueError:
                    payload = None
                if isinstance(payload, dict) and isinstance(payload.get("usage"), dict):
                    payload["usage"].pop("total_tokens", None)
                    out = b"data: " + json.dumps(payload, separators=(",", ":")).encode() + b"\n"
            self.wfile.write(b"%x\r\n" % len(out) + out + b"\r\n")
            self.wfile.flush()

        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()
        conn.close()


http.server.ThreadingHTTPServer(("0.0.0.0", LISTEN), Handler).serve_forever()
