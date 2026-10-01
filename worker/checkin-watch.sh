#!/bin/bash
# Claim this worker's vpc IP on the gateway, forever.
#
# The entrypoint used to try five times and then give up. A worker that
# booted during a gateway rebuild then 403'd every later post until the
# container was recreated. This loop never exits: backoff 2s → 30s with a
# little jitter while the gateway is down, and a slow re-checkin once it
# is up so a later state loss heals without a recreate.
set -u

if [ -z "${GATEWAY_URL:-}" ] || [ -z "${PISO_WORKER_NAME:-}" ] || [ -z "${PISO_WORKER_SLUG:-}" ]; then
  echo "piso-checkin-watch: GATEWAY_URL/PISO_WORKER_NAME/PISO_WORKER_SLUG must be set" >&2
  exit 3
fi

. /usr/local/lib/piso-gateway-post.sh

backoff=2
while true; do
  if piso_gateway_checkin; then
    echo "piso-checkin-watch: ok"
    backoff=2
    sleep 60
    continue
  fi
  jitter=$((RANDOM % 3))
  wait=$((backoff + jitter))
  echo "piso-checkin-watch: failed (retry in ${wait}s)"
  sleep "$wait"
  if [ "$backoff" -lt 30 ]; then
    backoff=$((backoff * 2))
    if [ "$backoff" -gt 30 ]; then
      backoff=30
    fi
  fi
done
