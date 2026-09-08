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

# When /plan starts Plannotator, ask the host to approve an ingress URL.
if [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -x /usr/local/bin/piso-planning-watch ]; then
  nohup /usr/local/bin/piso-planning-watch >/tmp/piso-planning-watch.log 2>&1 &
fi

# Publish every reachable dev-server port as an auto <slug>-<port>.piso.local
# route (the gateway reconciles the set; no host ceremony needed).
if [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -x /usr/local/bin/piso-ports-watch ]; then
  nohup /usr/local/bin/piso-ports-watch >/tmp/piso-ports-watch.log 2>&1 &
fi

# Report the worker's live context (folder / git repo / branch / commit /
# model) so the dashboard request log can label each row. Advisory only.
if [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -n "${PISO_WORKER_SLUG:-}" ] && [ -x /usr/local/bin/piso-context-watch ]; then
  nohup /usr/local/bin/piso-context-watch >/tmp/piso-context-watch.log 2>&1 &
fi

exec "$@"