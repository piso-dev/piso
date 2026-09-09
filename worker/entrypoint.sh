# piso worker entrypoint
# Installs the gateway's MITM CA into the system trust store (so curl/git/node
# all accept the gateway's certs), then execs the requested command
# (default: keep alive for `piso attach`).
set -e

if [ -f /piso-ca.pem ]; then
  cp /piso-ca.pem /usr/local/share/ca-certificates/piso-gateway.crt 2>/dev/null || true
  update-ca-certificates >/dev/null 2>&1 || true
fi

# pi checks ~/.pi/agent/bin/fd before PATH, then downloads a GitHub tarball
# and extracts with tar as root (chown uid 1001), which fails under cap_drop.
# The image ships fdfind; seed the agent bin so pi never downloads.
if [ ! -e /root/.pi/agent/bin/fd ]; then
  mkdir -p /root/.pi/agent/bin
  if [ -x /usr/local/bin/fd ]; then
    ln -sf /usr/local/bin/fd /root/.pi/agent/bin/fd
  elif [ -x /usr/bin/fdfind ]; then
    ln -sf /usr/bin/fdfind /root/.pi/agent/bin/fd
  fi
fi

# Seed baked extensions into the agent volume. The volume hides anything
# the image left under /root/.pi/agent, so the install lives in /opt/piso/npm.
if [ -f /opt/piso/npm/.piso-pkg-hash ]; then
  want=$(cat /opt/piso/npm/.piso-pkg-hash)
  got=""
  if [ -f /root/.pi/agent/.piso-pkg-hash ]; then
    got=$(cat /root/.pi/agent/.piso-pkg-hash)
  fi
  if [ "$want" != "$got" ] || [ ! -d /root/.pi/agent/npm/node_modules ]; then
    mkdir -p /root/.pi/agent
    # Volume leftovers can make a single rm -rf fail; retry the tree.
    rm -rf /root/.pi/agent/npm || true
    mkdir -p /root/.pi/agent/npm
    cp -a /opt/piso/npm/. /root/.pi/agent/npm/
    cp /opt/piso/npm/.piso-pkg-hash /root/.pi/agent/.piso-pkg-hash
  fi
fi

# node-gyp dev headers for the image's node version, baked into
# /opt/piso/node-gyp at build time. /root/.cache is a per-boot tmpfs here
# (compose tmpfs), so seed it on every start: with headers present, node-gyp's
# configure short-circuits and never extracts its header tarball — the extract
# does fchown for tarball uid/gid, which the kernel denies under cap_drop ALL
# (EPERM). This is the `TAR_ENTRY_ERROR EPERM: operation not permitted,
# fchown` failure seen when a native module (node-pty) is (re)built inside a
# hardened worker.
if [ -d /opt/piso/node-gyp ]; then
  mkdir -p /root/.cache/node-gyp
  cp -an /opt/piso/node-gyp/. /root/.cache/node-gyp/ 2>/dev/null || true
fi

# node-pty ships no linux prebuild; its install script is
# `node scripts/prebuild.js || node-gyp rebuild`, and npm re-runs it whenever
# pi (re)installs extensions into the agent volume. If the tree already has a
# compiled build/Release/pty.node but the platform prebuild is missing (volume
# baked from a pre-fix image), synthesize the prebuild so prebuild.js exits 0
# and node-gyp is never invoked in the hardened worker.
NP=/root/.pi/agent/npm/node_modules/node-pty
if [ -x "$NP/build/Release/pty.node" ]; then
  arch="$(node -e 'process.stdout.write(process.arch)' 2>/dev/null || true)"
  case "$arch" in
    arm64|x64) ;;
    *) arch="" ;;
  esac
  if [ -n "$arch" ] && [ ! -f "$NP/prebuilds/linux-$arch/pty.node" ]; then
    mkdir -p "$NP/prebuilds/linux-$arch"
    cp "$NP/build/Release/pty.node" "$NP/prebuilds/linux-$arch/pty.node"
  fi
fi

# Fold the Tier B informant convention into EVERY pi run via the global
# context file: pi reads ~/.pi/agent/AGENTS.md as a context file on every
# start (fresh, -r, --session, -p), so the model always knows piso-informant
# without any launcher passing flags or conversation-history stuffing.
# The image's /opt/piso/INFORMANT.md is the source of truth; the volume
# persists AGENTS.md, so re-sync whenever the content changes.
# settings.json "prompts" is NOT the contract — it only provides the
# interactive /INFORMANT cheat-sheet. Monitor excluded: its pi is the PM
# (MONITOR.md governs), and INFORMANT milestones would only pollute its feed.
if [ "${PISO_ROLE:-}" != "monitor" ] && [ -f /opt/piso/INFORMANT.md ]; then
  if [ ! -f /root/.pi/agent/AGENTS.md ] || ! cmp -s /opt/piso/INFORMANT.md /root/.pi/agent/AGENTS.md; then
    mkdir -p /root/.pi/agent
    cp /opt/piso/INFORMANT.md /root/.pi/agent/AGENTS.md
  fi
fi

# When /plan starts Plannotator, ask the host to approve an ingress URL.
if [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -x /usr/local/bin/piso-planning-watch ]; then
  nohup /usr/local/bin/piso-planning-watch >/tmp/piso-planning-watch.log 2>&1 &
fi

# Publish every reachable dev-server port as an auto <slug>-<port>.piso.local
# route (the gateway reconciles the set; no host ceremony needed).
if [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -x /usr/local/bin/piso-ports-watch ]; then
  nohup /usr/local/bin/piso-ports-watch >/tmp/piso-ports-watch.log 2>&1 &
fi

# Tier A activity informant: coarse beats (session start/end, alive) for the
# PM board's live/idle floor. Suppressed in the MONITOR role — the monitor's
# own pi process is a manager, not work-in-a-project, so its "working on …"
# beats would just be noise on its track (it has no project mount).
if [ "${PISO_ROLE:-}" != "monitor" ] && [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -x /usr/local/bin/piso-activity-watch ]; then
  nohup /usr/local/bin/piso-activity-watch >/tmp/piso-activity-watch.log 2>&1 &
fi

# Report the worker's live context (folder / git repo / branch / commit /
# model) so the dashboard request log can label each row. Advisory only.
if [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -n "${PISO_WORKER_SLUG:-}" ] && [ -x /usr/local/bin/piso-context-watch ]; then
  nohup /usr/local/bin/piso-context-watch >/tmp/piso-context-watch.log 2>&1 &
fi

exec "$@"