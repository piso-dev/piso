"""Local-only placeholder Git credential and tea profile tests."""
import base64
import json
import os
import shutil
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('git-access.py')


class GitAccessTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.rows = [dict(id='sec_one', provider='gitea', baseURL='https://git.example.test/gitea',
                         envKey='TEA_TOKEN', placeholder='piso_git_one')]
        self.env = dict(os.environ, PISO_PLACEHOLDERS_ENV=str(self.root / 'placeholders.env'),
                        PISO_GIT_HOME=str(self.root / 'git-access'),
                        GIT_CONFIG_GLOBAL=str(self.root / 'gitconfig'))
        self.write()

    def write(self):
        (self.root / 'placeholders.env').write_text('PISO_GIT_CONNECTIONS=' +
            base64.b64encode(json.dumps(self.rows).encode()).decode() + '\n')

    def run_script(self, *args, request=''):
        return subprocess.run(['python3', str(SCRIPT), *args], input=request, text=True,
                              capture_output=True, env=self.env, check=True).stdout

    def test_profiles_refresh_without_network(self):
        self.run_script('sync')
        profile = self.root / 'git-access' / 'tea' / 'tea' / 'config.yml'
        login = json.loads(profile.read_text())['logins'][0]
        self.assertEqual(login['token'], 'piso_git_one')
        self.assertEqual(login['name'], 'sec_one')
        self.assertTrue(login['default'])
        self.rows = []
        self.write()
        self.run_script('sync')
        self.assertEqual(json.loads(profile.read_text())['logins'], [])
        self.assertNotIn('git.example.test', (self.root / 'git-access' / 'gitconfig').read_text())

    @unittest.skipUnless(shutil.which("tea"), "requires tea")
    def test_real_tea_reads_placeholder_profile(self):
        self.run_script("sync")
        env = dict(self.env, XDG_CONFIG_HOME=str(self.root / "git-access" / "tea"))
        result = subprocess.run(["tea", "login", "list"], env=env, text=True, capture_output=True, check=True)
        self.assertIn("sec_one", result.stdout)
        self.assertNotIn("piso_git_one", result.stdout)

    def test_exact_https_destination_and_path(self):
        request = 'protocol=https\nhost=git.example.test\npath=gitea/org/repo.git\n\n'
        self.assertIn('password=piso_git_one', self.run_script('credential', 'get', request=request))
        for bad in [request.replace('https','http'), request.replace('git.example.test','evil.test'),
                    request.replace('gitea/org','other/org')]:
            self.assertEqual(self.run_script('credential','get',request=bad), '')

    def test_shared_host_requires_explicit_connection(self):
        self.rows.append(dict(self.rows[0], id='sec_two', placeholder='piso_git_two'))
        self.write()
        request = 'protocol=https\nhost=git.example.test\npath=gitea/org/repo.git\n\n'
        self.assertEqual(self.run_script('credential','get',request=request), '')
        result = self.run_script('credential','--connection','sec_two','get',request=request)
        self.assertIn('password=piso_git_two', result)
        self.assertEqual(self.run_script('credential','store',request=request),'')


if __name__ == '__main__':
    unittest.main()
