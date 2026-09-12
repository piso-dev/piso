#!/bin/bash
# piso-informant — the Tier B activity informant helper.
#
# The agent (pi) calls this to emit a SEMANTIC activity event at natural
# action boundaries, so the PM board shows the *why* of the work, not just
# that the worker is alive (which is Tier A's job):
#
#   piso-informant progress  "Started OAuth PKCE flow"
#   piso-informant milestone "Auth refactor merged"
#   piso-informant poke      "Blocked: OIDC discovery returns 404" [targetSlug]
#   piso-informant reminder  "Check PR by 5pm"
#   piso-informant waiting   "Need you to approve the schema"
#   piso-informant note      "spent the afternoon on the CQRS docs"
#
# Kinds are fixed (progress|milestone|reminder|poke|active|waiting|note); the text is
# free-form but sanitized gateway-side. The helper enforces shape, not content.
#
# Optionally pass a target project slug as the 3rd argument for pokes/
# reminders so the board renders the item on the TARGET project's track
# (the monitor uses this; a worker poking itself can omit it).
#
# Talks to the worker API (GATEWAY_URL, :8083) like the other watchers — never
# the host control plane. NO_PROXY covers "gateway" so curl goes direct.
set -u

gw="${GATEWAY_URL:-}"
worker="${PISO_WORKER_NAME:-}"
slug="${PISO_WORKER_SLUG:-}"

if [ -z "$gw" ] || [ -z "$worker" ] || [ -z "$slug" ]; then
  echo "piso-informant: GATEWAY_URL/PISO_WORKER_NAME/PISO_WORKER_SLUG not set" >&2
  exit 2
fi

kind="${1:-}"
text="${2:-}"
target="${3:-}"

case "$kind" in
  progress|milestone|reminder|poke|active|waiting|note) ;;
  *) echo "usage: piso-informant <progress|milestone|reminder|poke|active|waiting|note> <text> [targetSlug]" >&2; exit 2 ;;
esac
if [ -z "$text" ]; then
  echo "usage: piso-informant <kind> <text> [targetSlug]" >&2
  exit 2
fi

# json_esc makes a value safe for embedding in JSON (same as the watchers).
json_esc() {
  local v="$1"
  v=$(printf '%s' "$v" | tr '\r\n\t' '   ')
  v="${v//\\/\\\\}"
  v="${v//\"/\\\"}"
  printf '%s' "$v"
}

# Build the payload piecewise; the trailing comma must appear only when a
# target is present. printf '%s' protects against embedded % in text.
body="{\"worker\":\"$(json_esc "$worker")\",\"slug\":\"$(json_esc "$slug")\",\"kind\":\"$kind\",\"text\":\"$(json_esc "$text")\""
if [ -n "$target" ]; then
  body="$body,\"targetSlug\":\"$(json_esc "$target")\""
fi
body="$body}"

# Spool: local buffer for events that cannot reach the gateway right now.
# piso-activity-watch drains it once the gateway is reachable again, so a
# gateway blip never loses Tier B activity. Lives on the agent volume
# (writable + persistent in every worker, incl. the read-only-rootfs
# monitor). Bounded: the newest SPOOL_MAX lines win.
SPOOL="${PISO_SPOOL_FILE:-/root/.pi/agent/spool/activity.jsonl}"
SPOOL_MAX=200

# spool_append appends one fully-built JSON body under the shared lock (the
# same lock piso-activity-watch holds while draining).
spool_append() {
  mkdir -p "$(dirname "$SPOOL")" 2>/dev/null || return 1
  {
    if ! flock -w 3 9 2>/dev/null; then
      printf '%s\n' "$1" >>"$SPOOL" 2>/dev/null || return 1
      return 0
    fi
    printf '%s\n' "$1" >>"$SPOOL" 2>/dev/null || return 1
    n=$(wc -l <"$SPOOL" 2>/dev/null || echo 0)
    if [ "${n:-0}" -gt "$SPOOL_MAX" ]; then
      tail -n "$SPOOL_MAX" "$SPOOL" >"$SPOOL.tmp" 2>/dev/null && mv "$SPOOL.tmp" "$SPOOL" 2>/dev/null || true
    fi
  } 9>"$SPOOL.lock"
  return 0
}

# post_or_spool posts the event; on anything but 200/201 it buffers locally so
# nothing is lost. Still exits 0 when spooled: the event is deferred, not lost,
# and the board is advisory — the agent must keep going either way.
post_or_spool() {
  local code
  code=$(curl -sS --max-time 3 --retry 1 --retry-connrefused --retry-delay 1 \
    -o /tmp/piso-informant-resp.json -w '%{http_code}' \
    -X POST "${gw}/api/v1/worker/activity" \
    -H 'Content-Type: application/json' \
    -d "$body" || true)
  if [ "${code}" = "200" ] || [ "${code}" = "201" ]; then
    echo "informant ${code} $(tr -d '\n' < /tmp/piso-informant-resp.json 2>/dev/null)"
    return 0
  fi
  if spool_append "$body"; then
    echo "informant ${code} (spooled; activity-watch will retry)"
    return 0
  fi
  echo "informant ${code} (POST FAILED, spool unavailable: ${SPOOL})"
  return 1
}

post_or_spool