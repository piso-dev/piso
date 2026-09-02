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

RUN apt-get update \
  && apt-get install -y --no-install-recommends bash ca-certificates git ripgrep curl unzip \
  && rm -rf /var/lib/apt/lists/*

# Bun (pi's networking runtime)
RUN curl -fsSL https://bun.sh/install | bash -s "bun-v1.2.11" \
  && ln -s /root/.bun/bin/bun /usr/local/bin/bun

# pi CLI (same install the pi docs use; we invoke it via bun)
RUN npm install -g --ignore-scripts @earendil-works/pi-coding-agent

# Egress: only via the gateway. NO_PROXY keeps internal services direct.
ENV HTTPS_PROXY=http://gateway:8080 \
    HTTP_PROXY=http://gateway:8080 \
    NO_PROXY=gateway,localhost,127.0.0.1,*.piso.local \
    NODE_EXTRA_CA_CERTS=/piso-ca.pem

WORKDIR /workspace

COPY entrypoint.sh /usr/local/bin/piso-entrypoint
RUN chmod +x /usr/local/bin/piso-entrypoint

ENTRYPOINT ["/usr/local/bin/piso-entrypoint"]
CMD ["sleep", "infinity"]