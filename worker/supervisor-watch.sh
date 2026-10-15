#!/bin/bash
# Ask this worker's gateway to be designated "supervisor" of the Telegram front
# door, and keep asking.
#
# The gateway relays the ask to the coordinator, which grants exactly one
# standalone worker per published bot. This loop is the worker's side of that
# lease: it holds the designation while the coordinator keeps granting it, and
# writes the answer to a state file the agent can read.
#
# The ways the ask is refused are all normal answers, not errors:
#   not_standalone              this worker is not a launched cloud ship
#   gateway_no_telegram_lease   the gateway does not hold the Telegram poller
#   supervisor_taken            another worker is the supervisor
# It keeps polling through every refusal, so a later handoff is automatic.
set -u

. /usr/local/lib/piso-gateway-post.sh

gw="${GATEWAY_URL:-}"
worker="${PISO_WORKER_NAME:-}"
slug="${PISO_WORKER_SLUG:-}"

if [ -z "$gw" ] || [ -z "$worker" ] || [ -z "$slug" ]; then
  echo "piso-supervisor-watch: GATEWAY_URL/PISO_WORKER_NAME/PISO_WORKER_SLUG must be set" >&2
  exit 3
fi

STATE_DIR="${PISO_SUPERVISOR_DIR:-/root/.pi/agent/piso}"
STATE_FILE="${STATE_DIR}/supervisor.json"
STATE_PLAIN="${STATE_DIR}/supervisor.state"

# Renew well inside the coordinator's window; a refusal retries sooner, with
# jitter so a fleet does not stampede the gateway.
RENEW_SECS="${PISO_SUPERVISOR_RENEW_SECS:-40}"
RETRY_SECS="${PISO_SUPERVISOR_RETRY_SECS:-20}"

mkdir -p "$STATE_DIR" 2>/dev/null || true

json_esc() {
  local v="$1"
  v="${v//\\/\\\\}"
  v="${v//\"/\\\"}"
  printf '%s' "$v"
}

write_state() { # granted reason
  local granted="$1" reason="$2" plain
  if [ "$granted" = "1" ]; then
    plain="supervisor"
  else
    plain="not-supervisor:${reason:-unknown}"
  fi
  printf '%s\n' "$plain" > "$STATE_PLAIN" 2>/dev/null || true
  if [ "$granted" = "1" ]; then
    printf '{"worker":"%s","slug":"%s","granted":true,"reason":"","ts":"%s"}\n' \
      "$(json_esc "$worker")" "$(json_esc "$slug")" "$(date -u +%FT%TZ)" \
      > "$STATE_FILE" 2>/dev/null || true
  else
    printf '{"worker":"%s","slug":"%s","granted":false,"reason":"%s","ts":"%s"}\n' \
      "$(json_esc "$worker")" "$(json_esc "$slug")" "$(json_esc "$reason")" \
      "$(date -u +%FT%TZ)" > "$STATE_FILE" 2>/dev/null || true
  fi
}

while true; do
  payload="{\"worker\":\"$(json_esc "$worker")\",\"slug\":\"$(json_esc "$slug")\"}"
  code=$(piso_gateway_post "${gw}/api/v1/worker/supervisor" "$payload" /tmp/piso-supervisor.json)
  granted=0
  reason=""
  if [ "$code" = "200" ]; then
    if grep -Eq '"granted": *true' /tmp/piso-supervisor.json 2>/dev/null; then
      granted=1
    else
      reason=$(sed -n 's/.*"reason": *"\([^"]*\)".*/\1/p' /tmp/piso-supervisor.json 2>/dev/null)
    fi
  else
    reason="http-${code}"
  fi
  write_state "$granted" "$reason"
  if [ "$granted" = "1" ]; then
    sleep "$RENEW_SECS"
  else
    jitter=$((RANDOM % 10))
    sleep $((RETRY_SECS + jitter))
  fi
done
