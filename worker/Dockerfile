# piso worker — the AI agent's home. Runs pi (on Bun) with egress ONLY through
# the gateway (compose sets HTTPS_PROXY; piso_vpc is a Docker internal network).
# The gateway's CA is bind-mounted at /piso-ca.pem. Workers are read-only, so
# update-ca-certificates cannot run; SSL_CERT_FILE / GIT_SSL_CAINFO / etc.
# point at that file. Every HTTPS hop is MITM'd, so the piso CA is sufficient.
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
# legacy-peer-deps: host extensions independently pin @earendil-works/pi-ai
# (often as peerOptional). npm 7+ ERESOLVE-fails the bake when one package
# (e.g. pi-mcp-adapter@2.36) allows ^0.84–^0.86 while another already
# resolved 0.87. The host already runs this set; the worker must too.
# .npmrc is copied into the agent volume so a runtime reinstall does not
# hit the same conflict.
RUN cd /opt/piso/npm \
  && printf 'legacy-peer-deps=true\n' > .npmrc \
  && npm install --omit=dev --ignore-scripts --no-audit --no-fund \
       --legacy-peer-deps \
       --fetch-retries=5 --fetch-retry-mintimeout=20000 \
  && if [ -d node_modules/node-pty ]; then npm rebuild node-pty; fi \
  && if grep -qE '"dependencies"[[:space:]]*:[[:space:]]*\{[[:space:]]*"' package.json && [ ! -d node_modules ]; then \
       echo "piso: npm install produced no node_modules" >&2; exit 1; \
     fi \
  && sha256sum package.json | awk '{print substr($1,1,16)}' > .piso-pkg-hash

# node-pty ships no linux prebuild. When an extension pulled it in, ship the
# pty.node that `npm rebuild node-pty` just compiled so ANY runtime reinstall
# (pi's extension manager reinstalls into this tree) short-circuits at
# prebuild.js with exit 0 and never calls node-gyp. Inside a hardened worker
# (cap_drop ALL) node-gyp's header-tarball extraction attempts fchown for
# tarball uid/gid, which the kernel denies (EPERM) — this is the
# `TAR_ENTRY_ERROR EPERM: fchown` seen at image runtime.
# A fresh machine with empty host extensions has no node-pty: skip.
RUN if [ -d /opt/piso/npm/node_modules/node-pty ]; then \
      cd /opt/piso/npm \
      && test -f node_modules/node-pty/build/Release/pty.node \
      && PB="node_modules/node-pty/prebuilds/$(node -e 'process.stdout.write(process.platform + "-" + process.arch)')" \
      && mkdir -p "$PB" \
      && cp node_modules/node-pty/build/Release/pty.node "$PB/pty.node"; \
    fi

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

# bat/delta/glow: the hosted files-widget extension shells out to these (it
# `which`-checks them at startup, then runs bat/delta/glow to render files).
# Apt cannot cover them on bookworm: `bat` is 0.22.1 there (predates the
# --wrap=auto/--terminal-width flags files-widget passes) and git-delta/glow
# are not packaged at all. Pin the official static releases instead — the
# same versions verified against files-widget's exact invocations. Ship
# both arches since the image builds on arm64 or amd64 hosts.
# Must run BEFORE HTTPS_PROXY=gateway: `docker build` has real network
# (compose build network: host) but no `gateway` hostname, so curl would
# fail with "Could not resolve proxy: gateway".
RUN set -eu; \
  case "$(uname -m)" in \
    aarch64) RUST_ARCH=aarch64-unknown-linux-gnu; GLOW_ARCH=arm64; \
             BAT_SHA=422eb73e11c854fddd99f5ca8461c2f1d6e6dce0a2a8c3d5daade5ffcb6564aa; \
             DELTA_SHA=0bfce159a5cddd5feb3d6db4a616d883ff51253ce08ac7ec11cb1d208cfaab9e; \
             GLOW_SHA=810c39f4691feb75e675a5f5f54b9fa091354e7599d9cf9c9fc9b97e47a99759 ;; \
    x86_64)  RUST_ARCH=x86_64-unknown-linux-gnu; GLOW_ARCH=amd64; \
             BAT_SHA=726f04c8f576a7fd18b7634f1bbf2f915c43494c1c0f013baa3287edb0d5a2a3; \
             DELTA_SHA=8e695c5f586a8c53d6c3b01be0b4a422ed218bfed2a56191caebe373a1c18ab2; \
             GLOW_SHA=13e05e4b2acc18d2aee44291aefe6325b077ec321b631a0cfa780e8e3bc33f78 ;; \
    *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;; \
  esac; \
  D=/tmp/piso-tools; mkdir -p "$D"; cd "$D"; \
  curl -fsSL -o bat.tgz "https://github.com/sharkdp/bat/releases/download/v0.26.1/bat-v0.26.1-${RUST_ARCH}.tar.gz"; \
  curl -fsSL -o delta.tgz "https://github.com/dandavison/delta/releases/download/0.19.2/delta-0.19.2-${RUST_ARCH}.tar.gz"; \
  curl -fsSL -o glow.tgz "https://github.com/charmbracelet/glow/releases/download/v3.0.0/glow_3.0.0_Linux_${GLOW_ARCH}.tar.gz"; \
  echo "$BAT_SHA  bat.tgz" | sha256sum -c -; \
  echo "$DELTA_SHA  delta.tgz" | sha256sum -c -; \
  echo "$GLOW_SHA  glow.tgz" | sha256sum -c -; \
  tar xzf bat.tgz --no-same-owner;   install -m 0755 "bat-v0.26.1-${RUST_ARCH}/bat" /usr/local/bin/bat; \
  tar xzf delta.tgz --no-same-owner; install -m 0755 "delta-0.19.2-${RUST_ARCH}/delta" /usr/local/bin/delta; \
  tar xzf glow.tgz --no-same-owner;  install -m 0755 "glow_3.0.0_Linux_${GLOW_ARCH}/glow" /usr/local/bin/glow; \
  rm -rf "$D"

# Egress: only via the gateway. NO_PROXY keeps vpc services (worker API) direct.
# Lowercase + ALL_PROXY cover clients that ignore the uppercase pair.
# CA vars must be image/compose ENV (not entrypoint export): docker exec does
# not inherit the entrypoint's shell. Set after all RUN curls so build still
# uses the public trust store.
ENV HTTPS_PROXY=http://gateway:8080 \
    HTTP_PROXY=http://gateway:8080 \
    ALL_PROXY=http://gateway:8080 \
    https_proxy=http://gateway:8080 \
    http_proxy=http://gateway:8080 \
    all_proxy=http://gateway:8080 \
    NO_PROXY=gateway,localhost,127.0.0.1,*.piso.local \
    no_proxy=gateway,localhost,127.0.0.1,*.piso.local \
    SSL_CERT_FILE=/piso-ca.pem \
    CURL_CA_BUNDLE=/piso-ca.pem \
    GIT_SSL_CAINFO=/piso-ca.pem \
    REQUESTS_CA_BUNDLE=/piso-ca.pem \
    NODE_EXTRA_CA_CERTS=/piso-ca.pem \
    TMPDIR=/root/.pi/agent/tmp \
    NODE_COMPILE_CACHE=/root/.pi/agent/.node-compile-cache

WORKDIR /workspace

COPY entrypoint.sh /usr/local/bin/piso-entrypoint
COPY planning-watch.sh /usr/local/bin/piso-planning-watch
COPY loopback-forward.py /usr/local/bin/piso-loopback-forward
COPY context-watch.sh /usr/local/bin/piso-context-watch
COPY ports-watch.sh /usr/local/bin/piso-ports-watch
COPY activity-watch.sh /usr/local/bin/piso-activity-watch
COPY informant-help.sh /usr/local/bin/piso-informant
COPY INFORMANT.md /opt/piso/INFORMANT.md
COPY CAPABILITIES.md /opt/piso/CAPABILITIES.md
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