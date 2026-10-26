#!/usr/bin/env python3
"""Keep this worker's pi profile in step with the dashboard.

The gateway owns the profile: `models.json` carries every dashboard-managed
provider with a piso_ placeholder as its apiKey, and `settings.json` carries the
adopted agent settings plus the active default provider/model.

On a single host nobody needs to do anything: the gateway rewrites
$PISO_DATA/pi-profile in place and every worker mounts that directory read-only
at /opt/piso/pi-profile, so a dashboard change reaches the next pi session.

A cloud ship is a different machine. Its /opt/piso/pi-profile is an empty
directory Docker created for the bind mount, and nothing on the ship can write
it. Without this loop pi starts there with no provider at all, and the failure
only shows up later, as a model call that cannot resolve a key. So the worker
pulls the same two files over its own authenticated API and keeps its own copy
on the agent volume, then points ~/.pi/agent at that copy.

Details that matter:
- Files are replaced by rename, so a running pi never reads a half-written
  profile, and only when the bytes changed, so the volume is not churned.
- Nothing is written on a failed fetch: a gateway that is down must leave the
  last good profile alone rather than empty it.
- A gateway change is applied; a local edit (pi rewrites settings.json for a
  /model pick) is left alone until the gateway changes that file again. The
  last fetched digest is what tells the two apart.
- The symlinks are recreated on every successful fetch. That is what lets a
  worker that booted before the gateway came up pick up a profile later without
  being restarted.
- Exit code 3 means "not configured" (no GATEWAY_URL): the entrypoint's
  respawn treats it as a config error and backs off instead of hot-looping.
"""
from __future__ import print_function

import hashlib
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

AGENT_DIR = os.environ.get("PI_CODING_AGENT_DIR") or "/root/.pi/agent"
PROFILE_DIR = os.path.join(AGENT_DIR, "pi-profile")
MOUNT_DIR = "/opt/piso/pi-profile"
FILES = ("settings.json", "models.json")
STAMP_FILE = ".synced.json"
DEFAULT_INTERVAL = 60


def log(msg):
    print("piso-profile-sync: " + msg, flush=True)


def env_int(name, default):
    try:
        return int(os.environ.get(name) or default)
    except ValueError:
        return default


def ssl_context():
    # The entrypoint has already folded the piso CA and the public roots into
    # one bundle and exported it as SSL_CERT_FILE; fall back to openssl's own
    # default paths when it has not.
    cafile = (os.environ.get("SSL_CERT_FILE") or "").strip()
    if cafile and os.path.isfile(cafile):
        return ssl.create_default_context(cafile=cafile)
    return ssl.create_default_context()


def profile_url():
    gw = (os.environ.get("GATEWAY_URL") or "").strip().rstrip("/")
    worker = (os.environ.get("PISO_WORKER_NAME") or "").strip()
    if not gw:
        return ""
    url = gw + "/api/v1/worker/pi-profile"
    if worker:
        url += "?worker=" + urllib.parse.quote(worker)
    return url


def fetch(url, timeout=15):
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    token = (os.environ.get("PISO_WORKER_TOKEN") or "").strip()
    if token:
        req.add_header("Authorization", "Bearer " + token)
    # urllib honors HTTP(S)_PROXY, which points at the in-container egress
    # forwarder: it retries the upstream dial, so a gateway recreate is not a
    # failed profile update.
    with urllib.request.urlopen(req, timeout=timeout, context=ssl_context()) as resp:
        return json.loads(resp.read().decode("utf-8"))


def digest(data):
    return hashlib.sha256(data).hexdigest()[:16]


def read_stamp():
    try:
        with open(os.path.join(PROFILE_DIR, STAMP_FILE), "rb") as f:
            doc = json.loads(f.read().decode("utf-8"))
        return doc if isinstance(doc, dict) else {}
    except (OSError, ValueError):
        return {}


def write_stamp(stamp):
    write_atomic(os.path.join(PROFILE_DIR, STAMP_FILE), json.dumps(stamp, sort_keys=True).encode("utf-8"))


def write_atomic(path, data):
    try:
        with open(path, "rb") as f:
            if f.read() == data:
                return False
    except OSError:
        pass
    tmp = path + ".tmp"
    with open(tmp, "wb") as f:
        f.write(data)
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)
    return True


def link_profile():
    """Point ~/.pi/agent at the fetched copy.

    A previous boot may have linked the read-only host mount instead; the
    fetched copy is the one that changes, so it wins. pi reads these paths at
    session start, so the link is what makes a dashboard change stick.
    """
    for name in FILES:
        dest = os.path.join(AGENT_DIR, name)
        want = os.path.join(PROFILE_DIR, name)
        if os.path.islink(dest) and os.readlink(dest) == want:
            continue
        if os.path.isdir(dest) and not os.path.islink(dest):
            # Docker creates a directory when a bind-mount source is missing;
            # pi cannot use it and nothing is lost by removing it.
            os.rmdir(dest)
        elif os.path.exists(dest) or os.path.islink(dest):
            os.remove(dest)
        os.symlink(want, dest)


def sync_once():
    url = profile_url()
    if not url:
        log("GATEWAY_URL is not set")
        return 3
    try:
        doc = fetch(url)
    except urllib.error.HTTPError as e:
        if e.code in (404, 501):
            log("gateway has no pi-profile endpoint (HTTP %s); it predates cloud-ship "
                "workers -- upgrade it" % e.code)
            return 1
        log("fetch failed: HTTP %s" % e.code)
        return 1
    except Exception as e:
        log("fetch failed: %s" % e)
        return 1
    # An empty profile is a coordinator that has nothing to say yet, not an
    # instruction to erase a working one.
    payload = {}
    for name in FILES:
        body = doc.get(name[:-5]) or ""
        if not body.strip():
            log("profile has no %s; keeping the current copy" % name)
            return 1
        payload[name] = body.encode("utf-8")
    os.makedirs(PROFILE_DIR, mode=0o700, exist_ok=True)
    # Only a change on the gateway side is ours to write. A settings.json that
    # differs from the last fetch was edited here (pi writes it for a /model
    # pick or a theme change); reverting that once a minute would be a bug, not
    # reconciliation. The dashboard's next change still wins.
    stamp = read_stamp()
    changed = []
    for name in FILES:
        sha = digest(payload[name])
        if stamp.get(name) == sha and os.path.isfile(os.path.join(PROFILE_DIR, name)):
            continue
        if write_atomic(os.path.join(PROFILE_DIR, name), payload[name]):
            changed.append(name)
        stamp[name] = sha
    write_stamp(stamp)
    link_profile()
    if changed:
        log("%s updated (default %s/%s)" % (
            ", ".join(changed),
            doc.get("defaultProvider") or "-",
            doc.get("defaultModel") or "-",
        ))
    if doc.get("warning"):
        log("warning: %s" % doc["warning"])
    return 0


def main(argv):
    """--once makes a bounded attempt at boot; the default keeps it fresh."""
    once = "--once" in argv
    interval = max(15, env_int("PISO_PROFILE_SYNC_SECS", DEFAULT_INTERVAL))
    while True:
        rc = sync_once()
        if rc == 3:
            return 3
        if once:
            return rc
        time.sleep(interval)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
