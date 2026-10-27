"""Exercise pin bootstrap ordering and failures without Docker or network access."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

WORKER = Path(__file__).resolve().parent
SOURCE = (WORKER / 'entrypoint.sh').read_text()
FETCH = SOURCE[SOURCE.index('ensure_gateway_pin() {'):SOURCE.index('# Worker channel key.')]
START = SOURCE[SOURCE.index('# Local egress proxy:'):SOURCE.index('# Self-heal the slug')]


class GatewayPinTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=WORKER)
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.pin = self.root / 'gateway.json'
        self.script('piso-egress-forward', 'touch "$TEST_ROOT/proxy-ready"')
        self.script('piso-mcp', 'test -s "$TEST_ROOT/gateway.json"')
        self.script('curl', '''
[ -f "$TEST_ROOT/proxy-ready" ] || exit 7
printf '%s\\n' "$*" >>"$TEST_ROOT/curl-args"
out=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; shift; fi
  shift
done
if [ "$TEST_RESPONSE" = "401" ]; then printf 401; exit 0; fi
if [ "$TEST_RESPONSE" = "malformed" ]; then echo '{}' >"$out"; else
  printf '%s' '{"version":1,"x25519":"public","keyId":"test-key","signing":"public"}' >"$out"
fi
printf 200
''')
        self.env = dict(os.environ, TEST_ROOT=str(self.root), TEST_RESPONSE='200',
                        TMPDIR=str(self.root), PISO_LOG_DIR=str(self.root),
                        PISO_TASK_LOOP='1', PISO_EGRESS_UPSTREAM='https://gateway.example.com',
                        GATEWAY_URL='https://gateway.example.com', PISO_WORKER_TOKEN='test-token',
                        PATH=str(self.bin) + ':' + os.environ['PATH'])
        self.env.pop('PISO_MCP_PIN', None)

    def script(self, name, body):
        path = self.bin / name
        path.write_text('#!/bin/bash\n' + body + '\n')
        path.chmod(0o755)

    def boot(self):
        script = FETCH + START
        script = script.replace('/usr/local/bin/piso-mcp', str(self.bin / 'piso-mcp'))
        script = script.replace('/usr/local/bin/piso-egress-forward', str(self.bin / 'piso-egress-forward'))
        script = script.replace('/etc/piso/gateway.json', str(self.pin))
        script = script.replace('/etc/piso/token', str(self.root / 'token'))
        script = script.replace('(echo >/dev/tcp/127.0.0.1/18080)', 'test -f "$TEST_ROOT/proxy-ready"')
        return subprocess.run(['bash', '-ec', 'respawn() { "$1"; }\n' + script],
                              env=self.env, text=True, capture_output=True, timeout=10)

    def test_proxy_starts_before_authenticated_fetch(self):
        result = self.boot()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.pin.exists())
        args = (self.root / 'curl-args').read_text()
        self.assertIn('Authorization: Bearer test-token', args)
        self.assertIn('http://127.0.0.1:18080/mcp/key', args)

    def test_token_file_fallback(self):
        self.env['PISO_WORKER_TOKEN'] = ''
        (self.root / 'token').write_text('saved-token\n')
        result = self.boot()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Bearer saved-token', (self.root / 'curl-args').read_text())

    def test_rejected_or_malformed_pin_stops_task_loop(self):
        for response in ('401', 'malformed'):
            with self.subTest(response=response):
                self.env['TEST_RESPONSE'] = response
                result = self.boot()
                self.assertEqual(result.returncode, 3, result.stderr)
                self.assertFalse(self.pin.exists())
                self.assertIn('refusing to start the task loop', result.stderr)

    def test_existing_pin_is_not_refetched(self):
        self.pin.write_text('existing-pin')
        result = self.boot()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.pin.read_text(), 'existing-pin')
        self.assertFalse((self.root / 'curl-args').exists())


if __name__ == '__main__':
    unittest.main()
