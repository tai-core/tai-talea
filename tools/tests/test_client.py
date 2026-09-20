import contextlib
import hashlib
import hmac
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

spec = importlib.util.spec_from_file_location("talea_client", Path(__file__).parents[1] / "talea.py")
client = importlib.util.module_from_spec(spec)
spec.loader.exec_module(client)


class ClientTests(unittest.TestCase):
    def setUp(self):
        self.calls = []
        calls = self.calls
        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                body = self.rfile.read(int(self.headers['Content-Length']))
                calls.append((self.path, dict(self.headers), body))
                self.send_response(202)
                self.send_header('Content-Type', 'application/json')
                self.end_headers()
                self.wfile.write(b'{"accepted":true,"result":"APPLIED"}')
            def log_message(self, *args):
                pass
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.path = Path(directory.name) / 'client.json'
        self.cfg = dict(url='http://127.0.0.1:%d' % server.server_port,
                        admin_token='test-admin', partner_id='partner-a',
                        push_token='test-partner', push_secret='test-signing-secret')
        self.path.write_text(json.dumps(self.cfg))

    def run_cli(self, *args):
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(client.main(['--config', str(self.path), *args]), 0)

    def test_partner_revoke_signs_exact_request_and_keeps_retry_event_id(self):
        self.run_cli('push', 'revoke', 'node-a', '--grace', '120', '--event-id', 'event-42')
        path, headers, body = self.calls[0]
        self.assertEqual(path, '/v1/capacity/events')
        self.assertEqual(headers['Authorization'], 'Bearer test-partner')
        self.assertEqual(headers['X-Capacity-Signature'], hmac.new(
            b'test-signing-secret', headers['X-Capacity-Timestamp'].encode() + b'.' + body,
            hashlib.sha256).hexdigest())
        event = json.loads(body)
        self.assertEqual(event['event_id'], 'event-42')
        self.assertEqual(event['instance'], {'id': 'node-a'})
        self.assertEqual(event['grace_seconds'], 120)
        self.assertEqual(event['type'], 'CAPACITY_REVOKED')

    def test_operator_reclaim_uses_durable_admin_endpoint(self):
        self.run_cli('reclaim', 'node-a', '--grace', '0')
        path, headers, body = self.calls[0]
        self.assertEqual(path, '/v1/capacity/instances/node-a/reclaim')
        self.assertEqual(headers['Authorization'], 'Bearer test-admin')
        self.assertEqual(json.loads(body), {'grace_seconds': 0})

    def test_partner_add_accepts_capacity_json_without_admin_credentials(self):
        self.cfg.pop('admin_token')
        self.path.write_text(json.dumps(self.cfg))
        instance = {'id':'node-a','endpoint':'http://10.0.0.1:9001','service_endpoint':'http://10.0.0.1:9002','lease_id':'lease-a'}
        source = self.path.parent / 'node.json'
        source.write_text(json.dumps(instance))
        self.run_cli('push', 'add', '--file', str(source))
        event = json.loads(self.calls[0][2])
        self.assertEqual(event['instance'], instance)
        self.assertEqual(event['type'], 'CAPACITY_ADDED')


if __name__ == '__main__':
    unittest.main()
