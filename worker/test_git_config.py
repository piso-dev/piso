"""Exercise image and entrypoint Git setup with real gh/git, without network access."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

WORKER = Path(__file__).resolve().parent
SOURCE = (WORKER / 'entrypoint.sh').read_text()
SETUP = SOURCE[SOURCE.index('# Configure Git on the persistent volume'):SOURCE.index('# Remote workers need')]


@unittest.skipUnless(shutil.which('gh') and shutil.which('git'), 'requires gh and git')
class GitConfigTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=WORKER)
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.config = self.root / 'agent' / 'gitconfig'
        self.env = dict(os.environ)
        for key in list(self.env):
            if key.startswith(('GIT_CONFIG', 'GH_', 'GITHUB_')):
                self.env.pop(key)
        self.env.update(
            HOME=str(self.root / 'home'),
            GH_CONFIG_DIR=str(self.root / 'gh'),
            GIT_CONFIG_GLOBAL=str(self.config),
            GIT_CONFIG_NOSYSTEM='1',
            GH_TOKEN='piso_github_config_test',
        )
        Path(self.env['HOME']).mkdir()

    def setup_git(self, env=None):
        return subprocess.run(
            ['bash', '-ec', SETUP + '\nprintf "setup-completed\\n"'],
            env=env or self.env, cwd=self.root, text=True, capture_output=True,
            timeout=10,
        )

    def git(self, *args):
        return subprocess.check_output(
            ['git', 'config', '--global', *args], env=self.env,
            cwd=self.root, text=True,
        )

    def test_image_sets_default_for_exec_sessions(self):
        self.assertIn('ENV GIT_CONFIG_GLOBAL=/root/.pi/agent/gitconfig',
                      (WORKER / 'Dockerfile').read_text())
        self.assertIn('${GIT_CONFIG_GLOBAL:-/root/.pi/agent/gitconfig}', SETUP)

    def test_image_setup_without_runtime_credentials(self):
        dockerfile = (WORKER / 'Dockerfile').read_text()
        command = next(line.removeprefix('RUN ') for line in dockerfile.splitlines()
                       if line.startswith('RUN GIT_CONFIG_GLOBAL=/etc/gitconfig '))
        system_config = self.root / 'system-gitconfig'
        command = command.replace('/etc/gitconfig', '"$GIT_CONFIG_SYSTEM"')
        env = dict(self.env, GIT_CONFIG_SYSTEM=str(system_config))
        env.pop('GH_TOKEN')
        env.pop('GIT_CONFIG_NOSYSTEM')
        result = subprocess.run(
            ['bash', '-ec', command], env=env, cwd=self.root,
            text=True, capture_output=True, timeout=10,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('build-only-not-a-credential', system_config.read_text())
        self.assertNotIn('piso_', system_config.read_text())
        # An empty mounted agent volume must not hide the baked helpers.
        self.config.parent.mkdir()
        self.config.write_text('')
        for host in ('github.com', 'gist.github.com'):
            helpers = subprocess.check_output(
                ['git', 'config', '--get-all', f'credential.https://{host}.helper'],
                env=env, cwd=self.root, text=True,
            ).splitlines()
            self.assertEqual(helpers, ['', f'!{shutil.which("gh")} auth git-credential'])

    def test_setup_and_repeated_boot_preserve_settings(self):
        first = self.setup_git()
        self.assertEqual(first.returncode, 0, first.stderr)
        self.assertTrue(self.config.exists(), first.stderr)
        self.assertFalse((Path(self.env['HOME']) / '.gitconfig').exists())
        self.git('user.name', 'Existing User')
        before = self.config.read_text()
        second = self.setup_git()
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(self.config.read_text(), before)
        self.assertEqual(self.git('--get', 'user.name').strip(), 'Existing User')
        for host in ('github.com', 'gist.github.com'):
            helpers = self.git('--get-all', f'credential.https://{host}.helper').splitlines()
            self.assertEqual(helpers, ['', f'!{shutil.which("gh")} auth git-credential'])

    def test_placeholder_is_not_written_to_config(self):
        env = dict(self.env, GH_TOKEN='piso_github_config_test')
        result = self.setup_git(env)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn(env['GH_TOKEN'], self.config.read_text())

    def test_missing_token_warns_but_does_not_abort_startup(self):
        env = dict(self.env)
        env.pop('GH_TOKEN')
        result = self.setup_git(env)
        self.assertEqual(result.returncode, 0)
        self.assertIn('credential-helper setup failed', result.stderr)
        self.assertIn('setup-completed', result.stdout)

    def test_failed_setup_warns_but_does_not_abort_startup(self):
        # A file as the parent produces a deterministic error even as root.
        blocked = self.root / 'not-a-directory'
        blocked.write_text('')
        env = dict(self.env, GIT_CONFIG_GLOBAL=str(blocked / 'gitconfig'))
        result = self.setup_git(env)
        self.assertEqual(result.returncode, 0)
        self.assertIn('credential-helper setup failed', result.stderr)
        self.assertIn('setup-completed', result.stdout)


if __name__ == '__main__':
    unittest.main()
