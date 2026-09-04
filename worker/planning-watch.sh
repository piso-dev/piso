#!/bin/bash
# Notify the gateway when Plannotator binds. Do not cancel when the port
# drops: submit_plan exits if no browser connects, and that would hide the
# dashboard chip before the host can approve.
# Talks to the worker API (GATEWAY_URL), never the host control plane or MITM.
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
  code=$(curl -sS --max-time 3 -o /tmp/piso-planning-notify.json -w '%{http_code}' \
    -X POST "${gw}/api/v1/worker/planning" \
    -H 'Content-Type: application/json' \
    -d "{\"kind\":\"planning\",\"worker\":\"${worker}\",\"slug\":\"${slug}\",\"port\":${port}}" \
    || true)
  echo "notify ${code} $(tr -d '\n' < /tmp/piso-planning-notify.json 2>/dev/null)"
  if [ "${code}" = "200" ] || [ "${code}" = "201" ]; then
    return 0
  fi
  return 1
}

while true; do
  if port_up; then
    if [ "$posted" -eq 0 ]; then
      if notify; then
        posted=1
      fi
    fi
  else
    posted=0
  fi
  sleep 0.4
done
