#!/bin/bash
# Notify the gateway when Plannotator binds, and cancel when it stops.
# Talks to the control plane (GATEWAY_URL), never the MITM egress proxy.
set -u

gw="${GATEWAY_URL:-}"
worker="${PISO_WORKER_NAME:-}"
slug="${PISO_WORKER_SLUG:-}"
port="${PLANNOTATOR_PORT:-19432}"

if [ -z "$gw" ] || [ -z "$worker" ]; then
  exit 0
fi

posted=0

port_up() {
  timeout 0.2 bash -c "echo >/dev/tcp/127.0.0.1/${port}" 2>/dev/null
}

notify() {
  curl -sS --max-time 3 -X POST "${gw}/api/v1/ingress/requests" \
    -H 'Content-Type: application/json' \
    -d "{\"kind\":\"planning\",\"worker\":\"${worker}\",\"slug\":\"${slug}\",\"port\":${port}}" \
    >/dev/null || true
}

cancel() {
  curl -sS --max-time 3 -X POST "${gw}/api/v1/ingress/requests/cancel" \
    -H 'Content-Type: application/json' \
    -d "{\"kind\":\"planning\",\"worker\":\"${worker}\"}" \
    >/dev/null || true
}

while true; do
  if port_up; then
    if [ "$posted" -eq 0 ]; then
      notify
      posted=1
    fi
  else
    if [ "$posted" -eq 1 ]; then
      cancel
      posted=0
    fi
  fi
  sleep 2
done
