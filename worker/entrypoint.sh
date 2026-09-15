# piso worker entrypoint
# Starts background watchers, then execs the requested command (default: keep
# alive for `piso attach`). MITM CA trust is compose/image ENV pointing at
# /piso-ca.pem — the rootfs is read-only, so update-ca-certificates cannot run.
set -e

# Persistent log dir for the background watchers. /var/log sits on the
# read-only rootfs (every worker and the monitor are `read_only: true`) and
# /tmp is tmpfs (lost on restart); the agent volume is the one writable,
# persistent location in every container. Logs here survive container
# restarts and are inspectable on the host via `piso exec`.
PISO_LOG_DIR="${PISO_LOG_DIR:-/root/.pi/agent/logs}"
mkdir -p "$PISO_LOG_DIR" 2>/dev/null || true

# respawn keeps one background watcher alive, restarting it whenever it exits
# and logging to a persistent path. Exit code 3 means a configuration error
# (missing env / role mismatch) — back off 30s instead of hot-looping.
respawn() {
  local bin="$1" log="$2"
  (
    while true; do
      "$bin" >>"$log" 2>&1
      rc=$?
      if [ "$rc" = "3" ]; then
        echo "$(date -u +%FT%TZ) $bin config error (exit 3); retry in 30s" >>"$log"
        sleep 30
      else
        echo "$(date -u +%FT%TZ) $bin exited ($rc); restarting in 3s" >>"$log"
        sleep 3
      fi
    done
  ) &
}

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

# Fold the Tier B informant convention AND the sandbox boundary contract into
# EVERY pi run via the global context file: pi reads ~/.pi/agent/AGENTS.md as
# a context file on every start (fresh, -r, --session, -p), so the model
# knows when to emit activity and where the sandbox ends — without any
# launcher passing flags or conversation-history stuffing.
# The image's /opt/piso/INFORMANT.md + /opt/piso/CAPABILITIES.md are the
# sources of truth; the volume persists AGENTS.md, so re-sync whenever the
# content changes. The monitor gets CAPABILITIES only: its pi is the PM
# (MONITOR.md governs), and INFORMANT milestones would only pollute its feed.
# settings.json "prompts" is NOT the contract — it only provides the
# interactive /INFORMANT cheat-sheet.
ctx=/tmp/piso-AGENTS.md
: >"$ctx"
if [ "${PISO_ROLE:-}" != "monitor" ] && [ -f /opt/piso/INFORMANT.md ]; then
  cat /opt/piso/INFORMANT.md >>"$ctx"
  printf '\n' >>"$ctx"
fi
if [ -f /opt/piso/CAPABILITIES.md ]; then
  cat /opt/piso/CAPABILITIES.md >>"$ctx"
fi
if [ -s "$ctx" ]; then
  if [ ! -f /root/.pi/agent/AGENTS.md ] || ! cmp -s "$ctx" /root/.pi/agent/AGENTS.md; then
    mkdir -p /root/.pi/agent
    cp "$ctx" /root/.pi/agent/AGENTS.md
  fi
fi
rm -f "$ctx"

# Self-heal the slug↔IP registry: claim this worker's identity on the
# gateway so a recreated container (new vpc IP) is not hit with 403 "identity
# mismatch" on every watcher/informant POST. Idempotent and non-fatal;
# retries briefly to cover a cold compose start while the gateway warms up.
if [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -n "${PISO_WORKER_SLUG:-}" ]; then
  for i in 1 2 3 4 5; do
    code=$(curl -sS --max-time 2 -o /dev/null -w '%{http_code}' \
      -X POST "${GATEWAY_URL}/api/v1/worker/checkin" \
      -H 'Content-Type: application/json' \
      -d "{\"worker\":\"${PISO_WORKER_NAME}\",\"slug\":\"${PISO_WORKER_SLUG}\"}" 2>/dev/null) || code=000
    if [ "$code" = "200" ]; then
      echo "piso: identity checkin ok"
      break
    fi
    echo "piso: identity checkin http $code (retry $i/5)"
    sleep 2
  done
fi

# When /plan starts Plannotator, ask the host to approve an ingress URL.
if [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -x /usr/local/bin/piso-planning-watch ]; then
  respawn /usr/local/bin/piso-planning-watch "$PISO_LOG_DIR/planning-watch.log"
fi

# Publish every reachable dev-server port as an auto <slug>-<port>.piso.local
# route (the gateway reconciles the set; no host ceremony needed).
if [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -x /usr/local/bin/piso-ports-watch ]; then
  respawn /usr/local/bin/piso-ports-watch "$PISO_LOG_DIR/ports-watch.log"
fi

# Tier A activity informant: coarse beats (session start/end, alive) for the
# PM board's live/idle floor. Suppressed in the MONITOR role — the monitor's
# own pi process is a manager, not work-in-a-project, so its "working on …"
# beats would just be noise on its track (it has no project mount).
if [ "${PISO_ROLE:-}" != "monitor" ] && [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -x /usr/local/bin/piso-activity-watch ]; then
  respawn /usr/local/bin/piso-activity-watch "$PISO_LOG_DIR/activity-watch.log"
fi

# Report the worker's live context (folder / git repo / branch / commit /
# model) so the dashboard request log can label each row. Advisory only.
if [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -n "${PISO_WORKER_SLUG:-}" ] && [ -x /usr/local/bin/piso-context-watch ]; then
  respawn /usr/local/bin/piso-context-watch "$PISO_LOG_DIR/context-watch.log"
fi

exec "$@"