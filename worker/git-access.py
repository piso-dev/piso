#!/usr/bin/env python3
"""Materialize placeholder-only Git/tea configuration; never log credentials."""
import base64
import json
import os
from pathlib import Path
import subprocess
import sys
import time
from urllib.parse import urlsplit


def connections():
    path = Path(os.environ.get('PISO_PLACEHOLDERS_ENV', '/etc/piso/placeholders.env'))
    value = ''
    try:
        for line in path.read_text().splitlines():
            if line.startswith('PISO_GIT_CONNECTIONS='):
                value = line.split('=', 1)[1]
    except FileNotFoundError:
        pass
    return json.loads(base64.b64decode(value)) if value else []


def sync():
    rows = connections()
    root = Path(os.environ.get('PISO_GIT_HOME', '/root/.pi/agent/git-access'))
    root.mkdir(parents=True, exist_ok=True)
    # JSON is YAML 1.2, and tea's YAML decoder accepts it. No login command:
    # tea login add would make an authenticated probe during boot.
    logins = [dict(name=r['id'], url=r['baseURL'], token=r['placeholder'],
                   default=False, user='piso', version_check=False)
              for r in rows if r['provider'] == 'gitea']
    if len(logins) == 1:
        logins[0]['default'] = True
    tea = root / 'tea' / 'tea'
    tea.mkdir(parents=True, exist_ok=True)
    target = tea / 'config.yml'
    temp = target.with_suffix('.tmp')
    temp.write_text(json.dumps(dict(logins=logins)))
    temp.chmod(0o600)
    temp.replace(target)
    # Dedicated included config is rewritten, never appended, so revoked
    # assignments disappear. The helper reads the current mount on every use.
    groups = {}
    for row in rows:
        groups.setdefault(row['baseURL'], []).append(row)
    config = []
    executable = os.environ.get('PISO_GIT_HELPER', '/usr/local/bin/piso-git-access')
    for base in sorted(groups):
        config += ['[credential ' + json.dumps(base) + ']', '    helper =',
                   '    helper = !' + executable + ' credential', '    useHttpPath = true']
    (root / 'gitconfig').write_text('\n'.join(config) + '\n')
    global_config = os.environ.get('GIT_CONFIG_GLOBAL', '/root/.pi/agent/gitconfig')
    subprocess.run(['git', 'config', '--file', global_config, '--replace-all',
                    'include.path', str(root / 'gitconfig'), r'.*/git-access/gitconfig$'], check=True)


def credential():
    if sys.argv[-1] != 'get':
        return
    request = dict(line.rstrip('\n').split('=', 1) for line in sys.stdin if '=' in line)
    selection = None
    if '--connection' in sys.argv:
        selection = sys.argv[sys.argv.index('--connection') + 1]
    selected = []
    for row in connections():
        base = urlsplit(row['baseURL'])
        prefix = base.path.strip('/')
        path = request.get('path', '').strip('/')
        if (request.get('protocol') == 'https' and request.get('host') == base.netloc
                and (not prefix or path == prefix or path.startswith(prefix + '/'))
                and (not selection or row['id'] == selection)):
            selected.append(row)
    if len(selected) != 1:
        return  # ambiguous hosts never silently pick a token
    token = selected[0]['placeholder']
    if not token.startswith('piso_') or '\n' in token:
        return
    print('username=piso\npassword=' + token)


if __name__ == '__main__':
    if len(sys.argv) > 1 and sys.argv[1] == 'credential':
        credential()
    elif len(sys.argv) > 1 and sys.argv[1] == 'watch':
        while True:
            try:
                sync()
            except Exception:
                print('piso: Git configuration sync failed', file=sys.stderr)
            time.sleep(10)
    else:
        sync()
