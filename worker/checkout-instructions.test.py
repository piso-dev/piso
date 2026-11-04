#!/usr/bin/env python3
"""Exercise the entrypoint's actual AGENTS assembly for guest/ship task modes.
No services, container control, or writes to the running worker's prompts.
"""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

WORKER = Path(__file__).resolve().parent


class CheckoutInstructionsTest(unittest.TestCase):
    def test_generated_instructions(self):
        entrypoint = (WORKER / "entrypoint.sh").read_text()
        start = entrypoint.index('ctx=/tmp/piso-AGENTS.md')
        end = entrypoint.index('rm -f "$ctx"', start) + len('rm -f "$ctx"')
        assembly = entrypoint[start:end]
        convention = '${PISO_WORKBENCH:-$HOME/.pi/agent/workbench}/checkouts/<host>/<owner>/<repo>'
        for hull in ('guest', 'cloud_ship'):
            for task_loop in ('0', '1'):
                with self.subTest(hull=hull, task_loop=task_loop), tempfile.TemporaryDirectory() as tmp:
                    agent = Path(tmp) / 'agent'
                    replacements = {'/opt/piso': str(WORKER),
                                    '/tmp/piso-AGENTS.md': str(Path(tmp) / 'context.md'),
                                    '/root/.pi/agent': str(agent)}
                    # One pass: substituted temp paths may contain /root/.pi/agent.
                    script = re.sub('|'.join(re.escape(key) for key in replacements),
                                    lambda match: replacements[match.group()], assembly)
                    subprocess.run(['bash', '-eu', '-c', script], check=True,
                                   env={**os.environ, 'PISO_HULL': hull,
                                        'PISO_ROLE': 'worker', 'PISO_TASK_LOOP': task_loop})
                    generated = (agent / 'AGENTS.md').read_text()
                    self.assertIn(convention, generated)
                    self.assertIn('## Git checkout storage', generated)
                    self.assertIn('host_request.resultPath', generated)
                    self.assertIn('checkouts rather than silently colliding', generated)
                    self.assertNotRegex(generated, r'/workspace/(?:<owner>|<name>|v1-mono|repo)')
                    self.assertNotRegex(generated, r'(?:git clone|gh repo clone)[^\n]*/workspace/')
                    self.assertNotRegex(generated, r'checkouts/<owner>/<repo>')
                    self.assertEqual('## Find the repository' in generated, task_loop == '1')
                    # The two hulls must receive identical storage rules.
                    sources = [(WORKER / name).read_text() for name in
                               ('CAPABILITIES.md', 'CAPABILITIES.ship.md')]
                    sections = [re.search(r'## Git checkout storage\n(.*?)\n## Where to install',
                                          source, re.S).group(1) for source in sources]
                    self.assertEqual(*sections)


if __name__ == '__main__':
    unittest.main()
