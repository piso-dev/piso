"""Control requests go to the forwarder's origin mux, not the egress proxy."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

WORKER = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('profile_sync', WORKER / 'pi-profile-sync.py')
profile = importlib.util.module_from_spec(spec)
spec.loader.exec_module(profile)
spec = importlib.util.spec_from_file_location('supervisor_session', WORKER / 'supervisor-session.py')
supervisor = importlib.util.module_from_spec(spec)
spec.loader.exec_module(supervisor)


class GatewayControlTests(unittest.TestCase):
    def setUp(self):
        self.requests = []
        requests = self.requests

        class Handler(BaseHTTPRequestHandler):
            def respond(self):
                body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
                requests.append((self.command, self.path, self.headers.get('Authorization'), body))
                payload = json.dumps({'settings': '{}', 'models': '{}'}).encode()
                self.send_response(200)
                self.send_header('Content-Length', str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            do_POST = respond
            do_GET = respond

            def log_message(self, *args):
                pass

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.env = dict(os.environ, GATEWAY_URL='https://gateway.example.test/gw/assignment/',
                        PISO_EGRESS_UPSTREAM='https://gateway.example.test/gw/assignment',
                        PISO_WORKER_TOKEN='test-worker-token', PISO_WORKER_NAME='worker-1',
                        PISO_EGRESS_LISTEN_PORT=str(self.server.server_port),
                        HTTP_PROXY='http://127.0.0.1:1', HTTPS_PROXY='http://127.0.0.1:1',
                        http_proxy='http://127.0.0.1:1', https_proxy='http://127.0.0.1:1',
                        ALL_PROXY='http://127.0.0.1:1', all_proxy='http://127.0.0.1:1',
                        NO_PROXY='', no_proxy='')

    def shell(self, command, **overrides):
        return subprocess.check_output(
            ['bash', '-euc', '. "$1"; ' + command, 'test', str(WORKER / 'gateway-post.sh')],
            env=dict(self.env, **overrides), text=True, timeout=5)

    def test_watcher_posts_use_loopback_without_proxy(self):
        with tempfile.TemporaryDirectory(dir=WORKER) as tmp:
            for endpoint in ('ports', 'activity', 'context', 'planning'):
                with self.subTest(endpoint=endpoint):
                    command = ('piso_gateway_post "${GATEWAY_URL%/}/api/v1/worker/' + endpoint +
                               '?test=1" \'{"worker":"worker-1"}\' "' + tmp + '/response"')
                    self.assertEqual(self.shell(command), '200')
                    method, path, auth, body = self.requests[-1]
                    self.assertEqual((method, path, auth),
                                     ('POST', '/api/v1/worker/' + endpoint + '?test=1', 'Bearer test-worker-token'))
                    self.assertEqual(json.loads(body), {'worker': 'worker-1'})

    def test_local_hull_and_unrelated_urls_are_not_rewritten(self):
        url = 'https://gateway.example.test/gw/assignment/api/v1/worker/ports'
        for overrides in ({'PISO_WORKER_TOKEN': ''}, {'PISO_EGRESS_UPSTREAM': ''}):
            self.assertEqual(self.shell('piso_gateway_control_url "' + url + '"', **overrides), url)
        for url in ('https://other.example/api/v1/worker/ports',
                    'https://gateway.example.test/gw/assignment/api/v1/workers',
                    'https://gateway.example.test/gw/assignment.evil/api/v1/worker/ports'):
            self.assertEqual(self.shell('piso_gateway_control_url "' + url + '"'), url)

    def test_profile_fetch_uses_loopback_without_proxy_or_local_tls(self):
        with patch.dict(os.environ, self.env), patch.object(profile, 'ssl_context', side_effect=AssertionError('local hop needs no TLS')):
            url = profile.profile_url()
            self.assertEqual(url, 'http://127.0.0.1:%s/api/v1/worker/pi-profile?worker=worker-1' % self.server.server_port)
            self.assertEqual(profile.fetch(url), {'settings': '{}', 'models': '{}'})
        self.assertEqual(self.requests[-1][:3],
                         ('GET', '/api/v1/worker/pi-profile?worker=worker-1', 'Bearer test-worker-token'))

    def test_supervisor_calls_use_loopback_without_proxy_or_local_tls(self):
        with patch.dict(os.environ, self.env), patch.object(supervisor, 'ssl_context', side_effect=AssertionError('local hop needs no TLS')):
            gateway = supervisor.Gateway(self.env['GATEWAY_URL'], 'worker-1', 'ship')
            self.assertEqual(gateway.base, 'http://127.0.0.1:%s' % self.server.server_port)
            calls = (
                (lambda: gateway.lease(), 'POST', '/api/v1/worker/supervisor'),
                (lambda: gateway.messages(25), 'GET', '/api/v1/worker/messages?worker=worker-1&wait=25'),
                (lambda: gateway.ack(['m1']), 'POST', '/api/v1/worker/messages/ack'),
                (lambda: gateway.reply(42, 'hello'), 'POST', '/api/v1/worker/reply'),
                (lambda: gateway.status(42, 'working'), 'POST', '/api/v1/worker/status'),
            )
            for call, method, path in calls:
                with self.subTest(path=path):
                    call()
                    self.assertEqual(self.requests[-1][:2], (method, path))
                    # Authentication is supplied by the forwarder, not this client.
                    self.assertIsNone(self.requests[-1][2])
                    if method == 'POST':
                        self.assertEqual(json.loads(self.requests[-1][3])['worker'], 'worker-1')
            with patch.object(gateway.opener, 'open', wraps=gateway.opener.open) as opened:
                gateway.messages(25)
                self.assertEqual(opened.call_args.kwargs['timeout'], 35)

    def test_local_supervisor_routing_is_unchanged(self):
        context = object()
        for missing in ('PISO_WORKER_TOKEN', 'PISO_EGRESS_UPSTREAM'):
            with self.subTest(missing=missing), patch.dict(os.environ, dict(self.env, **{missing: ''})):
                gateway = supervisor.Gateway('http://gateway:8083/', 'worker-1', 'ship', context=context)
                self.assertEqual(gateway.base, 'http://gateway:8083')
                self.assertIsNone(gateway.opener)
                self.assertIs(gateway.context, context)

    def test_local_profile_url_is_unchanged(self):
        with patch.dict(os.environ, dict(self.env, PISO_WORKER_TOKEN='', GATEWAY_URL='http://gateway:8083')):
            self.assertEqual(profile.profile_url(), 'http://gateway:8083/api/v1/worker/pi-profile?worker=worker-1')


if __name__ == '__main__':
    unittest.main()
