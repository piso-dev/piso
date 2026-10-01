# Shared worker → gateway POST helper. Sourced by the watchers and the
# informant. Not a standalone command.
#
# A 403 identity mismatch means this container's vpc IP is not registered
# yet (typical when the worker booted while the gateway was down). Check in
# once and retry the POST once. A 400 is a bad payload: do not retry it
# here. Transport failures return 000 so the caller can spool or wait for
# its own loop. Callers must not treat a non-200 as fatal to the process.

piso_gateway_post_once() {
  local url="$1" body="$2" out="$3" code
  code=$(curl -sS --max-time 3 -o "$out" -w '%{http_code}' \
    -X POST "$url" \
    -H 'Content-Type: application/json' \
    -d "$body" 2>/dev/null) || code=000
  printf '%s' "$code"
}

piso_gateway_identity_mismatch() {
  grep -q 'identity mismatch' "$1" 2>/dev/null
}

# piso_gateway_checkin claims this container's vpc IP. Exit 0 only on HTTP 200.
piso_gateway_checkin() {
  local gw="${GATEWAY_URL:-}" worker="${PISO_WORKER_NAME:-}" slug="${PISO_WORKER_SLUG:-}" code
  if [ -z "$gw" ] || [ -z "$worker" ] || [ -z "$slug" ]; then
    return 1
  fi
  code=$(curl -sS --max-time 3 -o /dev/null -w '%{http_code}' \
    -X POST "${gw}/api/v1/worker/checkin" \
    -H 'Content-Type: application/json' \
    -d "{\"worker\":\"${worker}\",\"slug\":\"${slug}\"}" 2>/dev/null) || code=000
  [ "$code" = "200" ]
}

# piso_gateway_post URL JSON_BODY OUTFILE
# Prints the HTTP status (000 on transport failure). Retries once after a
# successful checkin when the gateway reports identity mismatch.
piso_gateway_post() {
  local url="$1" body="$2" out="${3:-/tmp/piso-gateway-post.json}" code
  : >"$out" 2>/dev/null || true
  if [ -z "$url" ]; then
    printf '000'
    return 0
  fi
  code=$(piso_gateway_post_once "$url" "$body" "$out")
  if [ "$code" = "403" ] && piso_gateway_identity_mismatch "$out"; then
    if piso_gateway_checkin; then
      code=$(piso_gateway_post_once "$url" "$body" "$out")
    fi
  fi
  printf '%s' "$code"
}
