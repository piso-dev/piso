#!/bin/bash
# Report the worker's listening TCP ports to the gateway, so every reachable
# dev server gets an automatic <slug>-<port>.piso.local ingress route.
#
# Design notes
# - Same pattern as piso-planning-watch / piso-context-watch: in-container
#   loop, POST the FULL listener set on change + on a heartbeat, talks to the
#   worker API (GATEWAY_URL, :8083) — never the host control plane or the
#   MITM proxy. NO_PROXY covers "gateway" so curl goes direct.
# - The image has no ss/iproute2/lsof: listeners come straight from
#   /proc/net/tcp + /proc/net/tcp6 (kernel truth, zero deps). State 0A is
#   LISTEN; local_address is big-endian hex ip:port.
# - Only *reachable* listeners are routed: the gateway connects over the vpc,
#   so a loopback-only bind is invisible to it. 0.0.0.0 (v4) and :: (v6
#   dual-stack, reachable over IPv4) count; loopback / specific-IP / IPv6-only
#   binds do not. Unreachable listeners are still reported with a note so the
#   dashboard can hint at the fix.
# - The gateway reconciles routes against this full set (create missing /
#   age out stale), so a port that stops listening is cleaned up there —
#   never deleted here on a first blip (dev servers restart constantly).
set -u

gw="${GATEWAY_URL:-}"
worker="${PISO_WORKER_NAME:-}"
slug="${PISO_WORKER_SLUG:-}"

if [ -z "$gw" ] || [ -z "$worker" ] || [ -z "$slug" ]; then
  exit 0
fi

POLL_SECS=2
HEARTBEAT_SECS=10

# json_esc makes a value safe for embedding in the JSON payload.
json_esc() {
  local v="$1"
  v=$(printf '%s' "$v" | tr '\r\n\t' '   ')
  v="${v//\\/\\\\}"
  v="${v//\"/\\\"}"
  printf '%s' "$v"
}

# hex2dec converts a /proc/net numeric field (big-endian hex) to decimal.
hex2dec() {
  printf '%d' "0x$1" 2>/dev/null || printf '0'
}

# collect_listeners sets ports_all — the full listener set, one entry per
# line, sorted by port, as "port|note" (empty note = reachable). Entries must
# stay whole lines: notes contain spaces, so a space-joined list would be torn
# apart by word splitting.
collect_listeners() {
  local line hexaddr hexip hexport port note tmp="" seen="" file
  for file in /proc/net/tcp /proc/net/tcp6; do
    [ -r "$file" ] || continue
    while IFS= read -r line; do
      set -- $line
      [ $# -ge 4 ] || continue
      case "$1" in
        *[a-zA-Z]*) continue ;;            # header ("sl:")
      esac
      [ "$4" = "0A" ] || continue          # 0A = LISTEN
      hexaddr="$2"
      hexip="${hexaddr%%:*}"
      hexport="${hexaddr##*:}"
      [ -n "$hexport" ] || continue
      port=$(hex2dec "$hexport")
      [ "$port" -gt 0 ] || continue
      case " $seen " in
        *" $port "*) continue ;;           # dedupe v4/v6 tables
      esac
      seen="$seen $port"
      note=""
      if [ "${#hexip}" -eq 8 ]; then
        # IPv4. /proc prints the address little-endian, so the LAST byte
        # pair is 0x7F exactly for the 127.0.0.0/8 loopback block.
        case "$hexip" in
          00000000) ;;                                      # 0.0.0.0 → reachable
          *7F)      note="bound to loopback 127.0.0.1" ;;
          *)        note="bound to a specific address" ;;
        esac
      else
        # IPv6 (32 hex digits, network order): :: is the dual-stack wildcard;
        # ::1 and ULA/link-local binds are unreachable over the v4-only vpc.
        case "$hexip" in
          00000000000000000000000000000000) ;;              # :: dual-stack → reachable
          00000000000000000000000000000001) note="bound to loopback ::1" ;;
          *)                              note="IPv6-only bind (vpc is IPv4)" ;;
        esac
      fi
      tmp="$tmp$port|$note"$'\n'
    done < "$file"
  done
  ports_all=$(printf '%s' "$tmp" | sort -t'|' -k1 -n)
}

# build_payload prints the full-set JSON body for the current listeners.
build_payload() {
  local out='{"worker":"' first=1 port note entry
  out+="$(json_esc "$worker")"
  out+='","slug":"'; out+="$(json_esc "$slug")"
  out+='","ports":['
  while IFS= read -r entry; do
    [ -n "$entry" ] || continue
    port="${entry%%|*}"
    note="${entry#*|}"
    if [ "$first" -eq 1 ]; then first=0; else out+=','; fi
    out+='{"port":'$port
    if [ -n "$note" ]; then
      out+=',"reachable":false,"note":"'"$(json_esc "$note")"'"'
    else
      out+=',"reachable":true'
    fi
    out+='}'
  done << EOF
$ports_all
EOF
  out+=']}'
  printf '%s' "$out"
}

# notify POSTs the set; success iff the gateway answered 200.
notify() {
  local payload="$1" code
  code=$(curl -sS --max-time 3 -o /tmp/piso-ports-notify.json -w '%{http_code}' \
    -X POST "${gw}/api/v1/worker/ports" \
    -H 'Content-Type: application/json' \
    -d "$payload" || true)
  echo "notify ${code} $(tr -d '\n' < /tmp/piso-ports-notify.json 2>/dev/null)"
  [ "${code}" = "200" ]
}

last=""
last_post=0
while true; do
  collect_listeners
  payload="$(build_payload)"
  now=$(date +%s 2>/dev/null) || now=0
  if [ "$payload" != "$last" ] || [ $((now - last_post)) -ge "$HEARTBEAT_SECS" ]; then
    if notify "$payload"; then
      last="$payload"
      last_post=$now
    fi
  fi
  sleep "$POLL_SECS"
done