# piso worker — the AI agent's home. Runs pi on Bun, with egress ONLY through
# the gateway (compose sets HTTPS_PROXY + no masquerade on the internal net).
# The gateway's CA is injected read-only so Bun/curl trust the MITM cert.
#
# Hardening: read-only rootfs, dropped caps, no-new-privileges, init — applied
# in compose, not here.
FROM node:24-bookworm-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends bash ca-certificates git ripgrep curl \
  && rm -rf /var/lib/apt/lists/*

# pi CLI (same install the pi docs use in their Docker example)
RUN npm install -g --ignore-scripts @earendil-works/pi-coding-agent

# Trust the gateway CA (bind-mounted read-only by compose at /piso-ca.pem).
ENV NODE_EXTRA_CA_CERTS=/piso-ca.pem

# Egress: only via the gateway. NO_PROXY keeps internal services direct.
ENV HTTPS_PROXY=http://gateway:8080 \
    HTTP_PROXY=http://gateway:8080 \
    NO_PROXY=gateway,localhost,127.0.0.1,*.piso.local

WORKDIR /workspace

# Keep the container alive so `piso attach` (docker exec -it) can enter it.
CMD ["sleep", "infinity"]