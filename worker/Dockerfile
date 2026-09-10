# piso worker — the AI agent's home. Runs pi (on Bun) with egress ONLY through
# the gateway (compose sets HTTPS_PROXY; piso_vpc is a Docker internal network).
# The gateway's CA is injected read-only by compose and installed into the
# system trust store by entrypoint.sh so everything (curl/git/node/bun) accepts
# the MITM cert.
#
# WHY BUN: pi's CLI is a node shebang, but Node's fetch (undici) does NOT honor
# HTTPS_PROXY. Bun's native fetch does (verified empirically). Running pi under
# Bun makes ALL of pi's model traffic route through the gateway.
#
# Hardening: read-only rootfs, dropped caps, no-new-privileges, init — applied
# in compose, not here.
FROM node:24-bookworm-slim

# fd-find is baked in: pi's runtime download uses tar --same-owner, which
# fails under cap_drop ALL. PISO_WORKER_HASH is applied after the install
# layers so a hash-only change does not bust the apt/bun/npm cache.
RUN apt-get update \
  && apt-get install -y --no-install-recommends bash ca-certificates git ripgrep curl unzip fd-find python3 make g++ openssh-client \
  && ln -sf /usr/bin/fdfind /usr/local/bin/fd \
  && rm -rf /var/lib/apt/lists/*

# Bun (pi's networking runtime)
RUN curl -fsSL https://bun.sh/install | bash -s "bun-v1.2.11" \
  && ln -s /root/.bun/bin/bun /usr/local/bin/bun

# pi CLI (same install the pi docs use; we invoke it via bun).
# PISO_PI_VERSION is pinned per rollout (`piso update` rewrites the staged
# ARG); default is the last known-good so the baked image never installs a
# silently-newer pi.
ARG PISO_PI_VERSION=0.84.4
RUN npm install -g --ignore-scripts @earendil-works/pi-coding-agent@${PISO_PI_VERSION}

# Host pi extensions. Installed here (Docker build has real network) into
# /opt/piso/npm — NOT /root/.pi/agent, which a volume hides at runtime.
# entrypoint.sh copies this tree into the agent volume when the stamp differs.
# Must run BEFORE HTTPS_PROXY=gateway or `docker build` npm would target
# a proxy that does not exist yet.
COPY package.json /opt/piso/npm/package.json
# --ignore-scripts skips postinstall downloads (pi-browser Chromium, etc.)
# that hang `docker build`. node-pty still needs a compile.
RUN cd /opt/piso/npm \
  && npm install --omit=dev --ignore-scripts --no-audit --no-fund \
       --fetch-retries=5 --fetch-retry-mintimeout=20000 \
  && npm rebuild node-pty \
  && if grep -qE '"dependencies"[[:space:]]*:[[:space:]]*\{[[:space:]]*"' package.json && [ ! -d node_modules ]; then \
       echo "piso: npm install produced no node_modules" >&2; exit 1; \
     fi \
  && sha256sum package.json | awk '{print substr($1,1,16)}' > .piso-pkg-hash

# node-pty ships no linux prebuild. Ship the pty.node that `npm rebuild
# node-pty` just compiled so ANY runtime reinstall (pi's extension manager
# reinstalls into this tree) short-circuits at prebuild.js with exit 0 and
# never calls node-gyp. Inside a hardened worker (cap_drop ALL) node-gyp's
# header-tarball extraction attempts fchown for tarball uid/gid, which the
# kernel denies (EPERM) — this is the `TAR_ENTRY_ERROR EPERM: fchown` seen
# at image runtime.
RUN PB="node_modules/node-pty/prebuilds/$(node -e 'process.stdout.write(process.platform + "-" + process.arch)')" \
  && mkdir -p "/opt/piso/npm/$PB" \
  && cp "/opt/piso/npm/node_modules/node-pty/build/Release/pty.node" "/opt/piso/npm/$PB/pty.node"

# Bake node-gyp's dev headers for the image's exact node version so a native
# configure can skip the header-tarball download+extract (whose fchown is
# blocked under cap_drop ALL). `npm rebuild node-pty` above already installed
# them into /root/.cache/node-gyp; keep a fallback install and persist the
# dir under /opt/piso for entrypoint.sh to seed the per-boot tmpfs.
RUN NODEVER="$(node -v | tr -d v)" \
  && CACHE=/root/.cache/node-gyp \
  && { [ -f "$CACHE/$NODEVER/installVersion" ] || node /usr/local/lib/node_modules/npm/node_modules/node-gyp/bin/node-gyp.js install "v${NODEVER}" --ensure; } \
  && mkdir -p /opt/piso/node-gyp \
  && cp -a "$CACHE/." /opt/piso/node-gyp/

# Egress: only via the gateway. NO_PROXY keeps internal services direct.
ENV HTTPS_PROXY=http://gateway:8080 \
    HTTP_PROXY=http://gateway:8080 \
    NO_PROXY=gateway,localhost,127.0.0.1,*.piso.local \
    NODE_EXTRA_CA_CERTS=/piso-ca.pem

WORKDIR /workspace

COPY entrypoint.sh /usr/local/bin/piso-entrypoint
COPY planning-watch.sh /usr/local/bin/piso-planning-watch
COPY loopback-forward.py /usr/local/bin/piso-loopback-forward
COPY context-watch.sh /usr/local/bin/piso-context-watch
COPY ports-watch.sh /usr/local/bin/piso-ports-watch
COPY activity-watch.sh /usr/local/bin/piso-activity-watch
COPY informant-help.sh /usr/local/bin/piso-informant
COPY INFORMANT.md /opt/piso/INFORMANT.md
COPY MONITOR.md /opt/piso/MONITOR.md
COPY monitor-loop.sh /usr/local/bin/piso-monitor-loop
RUN chmod +x /usr/local/bin/piso-monitor-loop
# make the monitor loop invocable as a plain name (and pi can call it too)
RUN ln -sf /usr/local/bin/piso-monitor-loop /usr/local/bin/piso-monitor
RUN chmod +x /usr/local/bin/piso-entrypoint /usr/local/bin/piso-planning-watch /usr/local/bin/piso-loopback-forward /usr/local/bin/piso-context-watch /usr/local/bin/piso-ports-watch /usr/local/bin/piso-activity-watch /usr/local/bin/piso-informant
# piso-informant is CALLED by the agent (not a background watcher); ensure it
# is on PATH (entrypoint runs at /usr/local/bin which is on the default PATH).
ENV PATH=/usr/local/bin:$PATH

# Declared last so changing the hash only rewrites this label layer.
ARG PISO_WORKER_HASH=dev
LABEL piso.worker.hash=$PISO_WORKER_HASH

ENTRYPOINT ["/usr/local/bin/piso-entrypoint"]
CMD ["sleep", "infinity"]