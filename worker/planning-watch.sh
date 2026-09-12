#!/bin/bash
# Notify the gateway when Plannotator binds. Do not cancel when the port
# drops: submit_plan exits if no browser connects, and that would hide the
# dashboard chip before the host can approve.
# Talks to the worker API (GATEWAY_URL), never the host control plane or MITM.
#
# Plannotator listens on 127.0.0.1:19432 (remote mode). Ingress dials the
# vpc address, so while the port is up we also run piso-loopback-forward
# (vpc_ip:19432 → 127.0.0.1:19432).
set -u

gw="${GATEWAY_URL:-}"
worker="${PISO_WORKER_NAME:-}"
slug="${PISO_WORKER_SLUG:-}"
port="${PLANNOTATOR_PORT:-19432}"
fwd="${PISO_LOOPBACK_FORWARD:-/usr/local/bin/piso-loopback-forward}"

if [ -z "$gw" ] || [ -z "$worker" ]; then
  echo "piso-planning-watch: GATEWAY_URL/PISO_WORKER_NAME must be set" >&2
  exit 3
fi

posted=0
fwd_pid=0

port_up() {
  timeout 0.2 bash -c "echo >/dev/tcp/127.0.0.1/${port}" 2>/dev/null
}

ensure_fwd() {
  if [ "$fwd_pid" -ne 0 ]; then
    if kill -0 "$fwd_pid" 2>/dev/null; then
      return 0
    fi
    wait "$fwd_pid" 2>/dev/null || true
    fwd_pid=0
  fi
  if [ ! -f "$fwd" ]; then
    return 1
  fi
  python3 -u "$fwd" "$port" >/tmp/piso-loopback-forward.log 2>&1 &
  fwd_pid=$!
  if kill -0 "$fwd_pid" 2>/dev/null; then
    return 0
  fi
  fwd_pid=0
  return 1
}

stop_fwd() {
  if [ "$fwd_pid" -eq 0 ]; then
    return 0
  fi
  kill "$fwd_pid" 2>/dev/null || true
  wait "$fwd_pid" 2>/dev/null || true
  fwd_pid=0
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
    ensure_fwd || true
    if [ "$posted" -eq 0 ]; then
      if notify; then
        posted=1
      fi
    fi
  else
    stop_fwd
    posted=0
  fi
  sleep 0.4
done
