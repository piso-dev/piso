"""Exercise the entrypoint's CA bundle with real PEM parsing, without Docker."""
import os
from pathlib import Path
import ssl
import subprocess
import tempfile
import unittest

WORKER = Path(__file__).resolve().parent
SOURCE = (WORKER / 'entrypoint.sh').read_text()
BUNDLE = SOURCE[SOURCE.index('# Remote workers need'):SOURCE.index('# Pi profile:')]
CA_VARS = ('SSL_CERT_FILE', 'CURL_CA_BUNDLE', 'GIT_SSL_CAINFO',
           'REQUESTS_CA_BUNDLE', 'NODE_EXTRA_CA_CERTS')


class CABundleTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=WORKER)
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.ca = self.root / 'piso-ca.pem'
        self.system = self.root / 'system-ca.pem'
        for path in (self.ca, self.system):
            subprocess.run([
                'openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                '-keyout', str(self.root / (path.stem + '.key')),
                '-out', str(path), '-days', '1', '-subj', '/CN=' + path.stem,
                '-addext', 'basicConstraints=critical,CA:TRUE',
            ], check=True, capture_output=True, timeout=15)
        self.pem = self.ca.read_bytes().rstrip(b'\r\n')

    def boot(self):
        script = BUNDLE.replace('/piso-ca.pem', str(self.ca))
        script = script.replace('/etc/ssl/certs/ca-certificates.crt', str(self.system))
        script += '\nprintf "%s\\n" ' + ' '.join('"$' + v + '"' for v in CA_VARS)
        env = dict(os.environ, TMPDIR=str(self.root))
        env.update({v: str(self.ca) for v in CA_VARS})
        result = subprocess.run(['bash', '-ec', script], env=env,
                                capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.splitlines()

    def test_bundle_parses_with_or_without_trailing_newline(self):
        for ending in (b'', b'\n', b'\r\n'):
            with self.subTest(ending=ending):
                self.ca.write_bytes(self.pem + ending)
                paths = self.boot()
                bundle = self.root / 'piso-ca-bundle.pem'
                self.assertEqual(paths, [str(bundle)] * len(CA_VARS))
                # Same OpenSSL parser used by profile-sync's TLS context.
                ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
                ctx.load_verify_locations(cafile=str(bundle))
                self.assertEqual(ctx.cert_store_stats()['x509_ca'], 2)
                self.assertEqual(self.ca.read_bytes(), self.pem + ending)

    def test_missing_system_bundle_keeps_worker_ca(self):
        self.system.unlink()
        self.ca.write_bytes(self.pem)
        bundle = self.boot()[0]
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.load_verify_locations(cafile=bundle)
        self.assertEqual(ctx.cert_store_stats()['x509_ca'], 1)

    def test_missing_worker_ca_leaves_environment_unchanged(self):
        self.ca.unlink()
        self.assertEqual(self.boot(), [str(self.ca)] * len(CA_VARS))
        self.assertFalse((self.root / 'piso-ca-bundle.pem').exists())


if __name__ == '__main__':
    unittest.main()
