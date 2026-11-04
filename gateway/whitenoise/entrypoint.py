"""Single writer for persistent MLS state; Docker init forwards termination.

The lock fd survives exec and lives for exactly the connector process lifetime.
Acquire BEFORE touching the socket: a second container must not unlink it.
"""
import fcntl
import os
from pathlib import Path
import sys

def launch(home="/data", socket_dir="/run/whitenoise", binary="/usr/local/bin/wn-agent"):
    os.umask(0o077)
    Path(home).mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(home, 0o700)
    fd = os.open(str(Path(home) / ".piso-connector.lock"), os.O_CREAT | os.O_RDWR, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        os.close(fd)
        sys.exit("White Noise state is already in use; refusing a second writer")
    os.set_inheritable(fd, True)
    Path(socket_dir).mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(socket_dir, 0o700)
    # Upstream probes/reclaims stale sockets and refuses live sockets or files.
    # Do not unlink here: another state home might be using this socket volume.
    path = str(Path(socket_dir) / "agent.sock")
    os.execv(binary, [binary, "--home", home, "--socket", path,
                      "--socket-dir-mode", "0700", "--socket-mode", "0600"])

if __name__ == "__main__":
    launch()
