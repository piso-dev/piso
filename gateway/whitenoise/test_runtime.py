"""Local runtime contract tests, no Docker or upstream binary needed."""
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import socket
import tempfile
import threading
import unittest
from unittest.mock import patch


def load(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + '.py'))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


entrypoint, health = load('entrypoint'), load('health')


class RuntimeTest(unittest.TestCase):
    def test_second_writer_refused_before_touching_socket(self):
        with tempfile.TemporaryDirectory() as root:
            home, runtime = Path(root) / 'home', Path(root) / 'run'
            home.mkdir()
            runtime.mkdir()
            sentinel = runtime / 'agent.sock'
            sentinel.write_text('do not delete')
            with (home / '.piso-connector.lock').open('w') as held:
                fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
                old = os.umask(0o077)
                try:
                    with patch.object(entrypoint.os, 'execv') as execute:
                        with self.assertRaises(SystemExit):
                            entrypoint.launch(str(home), str(runtime))
                        execute.assert_not_called()
                finally:
                    os.umask(old)
            self.assertEqual(sentinel.read_text(), 'do not delete')

    def test_readiness_requires_correlated_protocol_reply(self):
        for kind, protocol, ident, expected in [
            ('account_list', 'marmot.agent-control.v2', 'piso-health', True),
            ('error', 'marmot.agent-control.v2', 'piso-health', False),
            ('account_list', 'v1', 'piso-health', False),
            ('account_list', 'marmot.agent-control.v2', 'wrong-id', False),
        ]:
            with tempfile.TemporaryDirectory() as root:
                path = str(Path(root) / 'agent.sock')
                with socket.socket(socket.AF_UNIX) as listener:
                    listener.bind(path)
                    listener.listen()
                    def reply():
                        conn, _ = listener.accept()
                        with conn, conn.makefile('rb') as reader:
                            request = json.loads(reader.readline())
                            self.assertEqual(request['type'], 'account_list')
                            conn.sendall(json.dumps({'type': kind, 'marmot_agent_control': protocol,
                                                     'id': ident, 'accounts': []}).encode() + b'\n')
                    thread = threading.Thread(target=reply)
                    thread.start()
                    self.assertEqual(health.ready(path), expected)
                    thread.join(5)
                    self.assertFalse(thread.is_alive())


if __name__ == '__main__':
    unittest.main()
