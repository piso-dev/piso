"""Exercise the actual shell helpers without starting a watcher or posting telemetry."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SOURCE = Path(__file__).with_name('context-watch.sh').read_text()
HELPERS = SOURCE[SOURCE.index('# Match Pi'):SOURCE.index('# notify POSTs')]


class ContextWatchTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.addCleanup(self.tmp.cleanup)
        self.agent = Path(self.tmp.name).resolve()

    def run_shell(self, command, role='monitor'):
        env = dict(os.environ, PI_CODING_AGENT_DIR=str(self.agent), PISO_ROLE=role)
        return subprocess.check_output(['bash', '-c', 'set -u\nslug=' + role + '\n' + HELPERS + '\n' + command], env=env, text=True).strip()

    def session(self, cwd, provider='piso', model='unbiased/pareto-26.10-preview', padding=False):
        directory = Path(self.run_shell('session_dir "$CWD"'.replace('$CWD', cwd)))
        directory.mkdir(parents=True, exist_ok=True)
        row = json.dumps(dict(type='model_change', provider=provider, modelId=model), separators=(',', ':'))
        (directory / 'session.jsonl').write_text(row + '\n' + ('x' * 70000 if padding else ''))

    def test_monitor_reads_its_session_not_workspace_and_keeps_it_between_wakes(self):
        self.session('/workspace', model='wrong-workspace-model')
        self.session(str(self.agent / 'monitor-work'), padding=True)
        result = self.run_shell('detect_pid() { :; }; ctx; printf "%s|%s" "$folder" "$model"')
        self.assertEqual(result, str(self.agent / 'monitor-work') + '|piso/unbiased/pareto-26.10-preview')

    def test_worker_still_reads_workspace(self):
        self.session('/workspace', model='worker-model')
        self.session(str(self.agent / 'monitor-work'), model='monitor-model')
        self.assertEqual(self.run_shell('model_ctx ""', role='worker'), 'piso/worker-model')

    def test_live_cwd_can_select_a_nested_project(self):
        self.session('/workspace/nested', model='nested-model')
        self.assertEqual(self.run_shell('model_ctx "" /workspace/nested', role='worker'), 'piso/nested-model')

    def test_no_session_does_not_claim_configured_default_was_used(self):
        (self.agent / 'settings.json').write_text(json.dumps(dict(defaultProvider='piso', defaultModel='not-yet-used')))
        self.assertEqual(self.run_shell('model_ctx ""'), '')


if __name__ == '__main__':
    unittest.main()
