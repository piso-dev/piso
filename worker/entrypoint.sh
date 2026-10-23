# piso worker entrypoint
# Starts background watchers, then execs the requested command (default: keep
# alive for `piso attach`). MITM CA trust is compose/image ENV pointing at
# /piso-ca.pem — the rootfs is read-only, so update-ca-certificates cannot run.
set -e

# Persistent dirs on the agent volume. /var/log sits on the read-only
# rootfs and /tmp is tmpfs (RAM-backed, wiped on restart). First `pi` start
# compiles host extensions (jiti) and would OOM the Docker VM if that cache
# landed in /tmp — keep it on the volume. docker exec does not inherit these
# exports; TMPDIR / NODE_COMPILE_CACHE are also image/compose ENV.
PISO_LOG_DIR="${PISO_LOG_DIR:-/root/.pi/agent/logs}"
TMPDIR="${TMPDIR:-/root/.pi/agent/tmp}"
NODE_COMPILE_CACHE="${NODE_COMPILE_CACHE:-/root/.pi/agent/.node-compile-cache}"
export TMPDIR NODE_COMPILE_CACHE
mkdir -p "$PISO_LOG_DIR" "$TMPDIR" "$NODE_COMPILE_CACHE" 2>/dev/null || true

# Configure Git on the persistent volume, not the read-only /root/.gitconfig.
# gh reads GH_TOKEN; Git subprocesses need this helper to send the placeholder
# to the gateway. setup-git is local-only and safe to repeat on every boot.
export GIT_CONFIG_GLOBAL="${GIT_CONFIG_GLOBAL:-/root/.pi/agent/gitconfig}"
if ! (mkdir -p "$(dirname "$GIT_CONFIG_GLOBAL")" && gh auth setup-git --hostname github.com); then
  echo "piso: GitHub credential-helper setup failed; Git authentication may be unavailable" >&2
fi

# Remote workers need the piso MITM CA *and* public CAs (coordinator Let's Encrypt).
if [ -f /piso-ca.pem ]; then
  bundle="$TMPDIR/piso-ca-bundle.pem"
  {
    cat /piso-ca.pem
    cat /etc/ssl/certs/ca-certificates.crt 2>/dev/null || true
  } >"$bundle" 2>/dev/null || true
  if [ -s "$bundle" ]; then
    export SSL_CERT_FILE="$bundle" CURL_CA_BUNDLE="$bundle" GIT_SSL_CAINFO="$bundle" REQUESTS_CA_BUNDLE="$bundle" NODE_EXTRA_CA_CERTS="$bundle"
  fi
fi

# Host pi-profile is a directory mount at /opt/piso/pi-profile. Do not bind-mount
# settings.json / models.json as files: if they are missing, Docker creates host
# directories, then later "mount a directory onto a file" fails against the
# agent volume. Symlink from the dir mount into the volume.
if [ -d /opt/piso/pi-profile ]; then
  mkdir -p /root/.pi/agent
  for f in settings.json models.json; do
    if [ -e "/opt/piso/pi-profile/$f" ]; then
      rm -rf "/root/.pi/agent/$f"
      ln -sfn "/opt/piso/pi-profile/$f" "/root/.pi/agent/$f"
    fi
  done
fi

# respawn keeps one background watcher alive, restarting it whenever it exits
# and logging to a persistent path. Exit code 3 means a configuration error
# (missing env / role mismatch) — back off 30s instead of hot-looping.
respawn() {
  local bin="$1" log="$2"; shift 2
  (
    while true; do
      "$bin" "$@" >>"$log" 2>&1
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

# Agent bin is ahead of /usr/local/bin on PATH. A pre-image bridge copy of
# piso-mcp-add there would shadow the baked helper forever; drop it once the
# image ships the command.
if [ -x /usr/local/bin/piso-mcp-add ] && [ -e /root/.pi/agent/bin/piso-mcp-add ]; then
  rm -f /root/.pi/agent/bin/piso-mcp-add
fi

# Worker channel key. A mounted remote identity is copied onto the agent
# volume; otherwise ensure generates one. A corrupt file is left untouched
# (ensure exits non-zero) so a bad key is never overwritten at boot.
if [ -x /usr/local/bin/piso-mcp ]; then
  mkdir -p /root/.pi/agent/piso 2>/dev/null || true
  if ! piso-mcp identity ensure >/dev/null; then
    echo "piso-mcp: worker identity ensure failed; channel will not start" >&2
  fi
  if [ -f /usr/local/lib/piso-mcp-merge.py ]; then
    python3 /usr/local/lib/piso-mcp-merge.py || echo "piso-mcp: mcp.json merge failed" >&2
  fi
fi

# Seed piso-owned global extensions. Keep these separate from the host npm
# extension tree: they ship with the worker image and are always available,
# while the symlink leaves the source on the read-only image filesystem.
if [ -d /opt/piso/extensions ]; then
  mkdir -p /root/.pi/agent/extensions
  # Remove only obsolete image-owned links (e.g. renamed bundled extensions).
  # Never delete user extensions or links to their own extension packages.
  for ext in /root/.pi/agent/extensions/*; do
    [ -L "$ext" ] || continue
    target=$(readlink "$ext")
    case "$target" in
      /opt/piso/extensions/*) [ -e "$target" ] || rm -f "$ext" ;;
    esac
  done
  for ext in /opt/piso/extensions/*.ts /opt/piso/extensions/*.js; do
    [ -e "$ext" ] || continue
    ln -sfn "$ext" "/root/.pi/agent/extensions/$(basename "$ext")"
  done
fi

# Seed baked host extensions into the agent volume. The volume hides anything
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

# Fold the opening-moves work loop, the Tier B informant convention, AND the
# sandbox boundary contract into EVERY pi run via the global context file: pi
# reads ~/.pi/agent/AGENTS.md as a context file on every start (fresh, -r,
# --session, -p), so the model knows the reflex at task start, when to emit
# activity, and where the sandbox ends — without any launcher passing flags or
# conversation-history stuffing.
# The image's /opt/piso/WORKLOOP.md + /opt/piso/INFORMANT.md +
# /opt/piso/CAPABILITIES.md are the sources of truth; the volume persists
# AGENTS.md, so re-sync whenever the content changes. WORKLOOP leads the file:
# a decision procedure is most likely to fire when it is read first, before the
# tool catalog. The monitor gets CAPABILITIES only: its pi is the PM
# (MONITOR.md governs), and neither discovery moves nor INFORMANT milestones
# belong on its feed.
# settings.json "prompts" is NOT the contract — it only provides the
# interactive /INFORMANT cheat-sheet.
ctx=/tmp/piso-AGENTS.md
: >"$ctx"
if [ "${PISO_ROLE:-}" != "monitor" ] && [ -f /opt/piso/WORKLOOP.md ]; then
  cat /opt/piso/WORKLOOP.md >>"$ctx"
  printf '\n' >>"$ctx"
fi
if [ "${PISO_ROLE:-}" != "monitor" ] && [ -f /opt/piso/INFORMANT.md ]; then
  cat /opt/piso/INFORMANT.md >>"$ctx"
  printf '\n' >>"$ctx"
fi
if [ -f /opt/piso/CAPABILITIES.md ]; then
  cat /opt/piso/CAPABILITIES.md >>"$ctx"
fi
if [ "${PISO_TASK_LOOP:-}" = "1" ] && [ "${PISO_ROLE:-}" != "monitor" ] && [ -f /opt/piso/TASKLOOP.md ]; then
  printf '\n' >>"$ctx"
  cat /opt/piso/TASKLOOP.md >>"$ctx"
fi
if [ -s "$ctx" ]; then
  if [ ! -f /root/.pi/agent/AGENTS.md ] || ! cmp -s "$ctx" /root/.pi/agent/AGENTS.md; then
    mkdir -p /root/.pi/agent
    cp "$ctx" /root/.pi/agent/AGENTS.md
  fi
fi
rm -f "$ctx"

# Local egress proxy: retries the dial to gateway:8080 for ~45s so a
# gateway recreate does not fail in-flight model calls. Must be up before
# exec. HTTP(S)_PROXY points at 127.0.0.1:18080; this process dials the
# real gateway and does not replay a request the gateway has already accepted.
if [ -x /usr/local/bin/piso-egress-forward ]; then
  respawn /usr/local/bin/piso-egress-forward "$PISO_LOG_DIR/egress-forward.log"
  # HTTP_PROXY already points here; wait briefly so the first pi call is not
  # a connection-refused race against the just-spawned listener.
  for _i in 1 2 3 4 5 6 7 8 9 10; do
    (echo >/dev/tcp/127.0.0.1/18080) >/dev/null 2>&1 && break
    sleep 0.2
  done
fi

# Self-heal the slug↔IP registry for the life of the container, not just
# five tries at boot. A worker that starts while the gateway is rebuilding
# would otherwise 403 every later post until it was recreated.
if [ -n "${PISO_PUBLIC_URL_FILE:-}" ]; then
  mkdir -p "$(dirname "$PISO_PUBLIC_URL_FILE")" 2>/dev/null || true
  export PISO_PUBLIC_URL_FILE
fi
if [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -n "${PISO_WORKER_SLUG:-}" ] && [ -x /usr/local/bin/piso-checkin-watch ]; then
  respawn /usr/local/bin/piso-checkin-watch "$PISO_LOG_DIR/checkin-watch.log"
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

# Unattended inbox loop: peek every ≤5 min when pi is down, one session per
# task, never preempt. Off unless PISO_TASK_LOOP=1 (cloud ships).
if [ "${PISO_TASK_LOOP:-}" = "1" ] && [ "${PISO_ROLE:-}" != "monitor" ] && [ -x /usr/local/bin/piso-task-watch ]; then
  respawn /usr/local/bin/piso-task-watch "$PISO_LOG_DIR/task-watch.log"
fi

# Standalone (cloud ship) workers compete for the one supervisor lease on the
# gateway's Telegram front door. The coordinator refuses a local module, so this
# only runs where PISO_TASK_LOOP marks a launched worker. The session renews the
# lease, long-polls the gateway mailbox, and drives one persistent pi RPC
# session, so a Telegram message joins the running conversation.
if [ "${PISO_TASK_LOOP:-}" = "1" ] && [ -n "${GATEWAY_URL:-}" ] && [ -n "${PISO_WORKER_NAME:-}" ] && [ -n "${PISO_WORKER_SLUG:-}" ] && [ -x /usr/local/bin/piso-supervisor-session ]; then
  respawn /usr/local/bin/piso-supervisor-session "$PISO_LOG_DIR/supervisor-session.log"
fi

# Tinfoil local proxy: plaintext HTTP to 127.0.0.1:3301, sealed (EHBP) to an
# attested router enclave, so the model API key only ever leaves as a piso
# placeholder the gateway substitutes. Started ONLY when this worker actually
# holds a tinfoil placeholder — the var name comes from the secret's envKey
# (DeriveEnvKey("tinfoil") = TINFOIL, DeriveEnvKey("tinfoil api key") =
# TINFOIL_API_KEY), so match on the value, not a fixed name. Without a bound
# secret the proxy would just add a listener and a failing attestation fetch.
TINFOIL_KEY_VAR=""
while IFS='=' read -r _k _v; do
  case "$_k" in
    *TINFOIL*) case "$_v" in piso_*) TINFOIL_KEY_VAR="$_k" ;; esac ;;
  esac
done < <(env)

if [ -n "$TINFOIL_KEY_VAR" ] && [ -x /usr/local/bin/tinfoil-proxy ]; then
  # Prompt-cache secret: the proxy otherwise writes ~/.tinfoil/user_cache_secret,
  # which lands on the read-only rootfs (HOME=/root) and silently falls back to
  # an in-memory value — every restart would then lose cache continuity. Pin one
  # on the persistent agent volume instead.
  TINFOIL_STATE=/root/.pi/agent/tinfoil
  mkdir -p "$TINFOIL_STATE" 2>/dev/null || true
  if [ ! -s "$TINFOIL_STATE/user_cache_secret" ]; then
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$TINFOIL_STATE/user_cache_secret" \
      && chmod 600 "$TINFOIL_STATE/user_cache_secret"
  fi
  if [ -n "${TINFOIL_USER_CACHE_SECRET:-}" ]; then
    export TINFOIL_USER_CACHE_SECRET
  elif [ -s "$TINFOIL_STATE/user_cache_secret" ]; then
    TINFOIL_USER_CACHE_SECRET="$(cat "$TINFOIL_STATE/user_cache_secret")"
    export TINFOIL_USER_CACHE_SECRET
  fi
  respawn /usr/local/bin/tinfoil-proxy "$PISO_LOG_DIR/tinfoil-proxy.log" \
    --bind 127.0.0.1 --port 3301 --log-format json
  # Attestation + router discovery take a moment; wait so the agent's first
  # model call is not a connection-refused race against the fresh listener.
  for _i in $(seq 1 50); do
    (echo >/dev/tcp/127.0.0.1/3301) >/dev/null 2>&1 && break
    sleep 0.2
  done
fi

# Signal that seeding is done so `piso up` / `piso attach` can wait. /run is
# tmpfs; gone on restart, rewritten every boot. Attach before this exists
# races the ~650MB extension copy and first-start compile (exit 137).
: >/run/piso-ready 2>/dev/null || true

exec "$@"