#!/bin/bash
# Tier A activity informant: derive COARSE activity from the worker without the
# agent's help, and post it to the gateway's activity store. The PM board's
# "floor" (alive / idle / session start) comes from here; the semantic richness
# (progress / milestones / pokes) comes from Tier B (the agent itself).
#
# Design notes
# - Same pattern as context-watch / ports-watch: in-container loop, POST to the
#   worker API (GATEWAY_URL, :8083) — never the host control plane.
# - Events: kind=active (alive beat w/ project context), kind=idle (pi is up
#   but not working for ≥ IDLE_SECS — one row per idle stretch), kind=note
#   (session start/end). Idle-at-prompt is NOT active; `waiting` is still
#   Tier B only (the agent asked a question). `idle` is the positive signal
#   the monitor pokes on: session attached, nobody working.
# - Sanitization identical to context-watch (control chars → space, cap len).
set -u

. /usr/local/lib/piso-gateway-post.sh

gw="${GATEWAY_URL:-}"
worker="${PISO_WORKER_NAME:-}"
slug="${PISO_WORKER_SLUG:-}"

if [ -z "$gw" ] || [ -z "$worker" ] || [ -z "$slug" ]; then
  echo "piso-activity-watch: GATEWAY_URL/PISO_WORKER_NAME/PISO_WORKER_SLUG must be set" >&2
  exit 3
fi

BEAT_SECS=30             # store an active heartbeat at most every 30 s
IDLE_SECS=600            # pi up + not working for this long → one idle row
RESCAN_SECS=3            # check for session boundary/beat every 3 s
GIT=(git -c safe.directory=*)

san() {
  local v="$1" max="${2:-500}"
  v=$(printf '%s' "$v" | tr '\r\n\t' '   ')
  v="${v//\\/\\\\}"
  v="${v//\"/\\\"}"
  if [ "${#v}" -gt "$max" ]; then
    v="${v:0:$max}"
  fi
  printf '%s' "$v"
}

# Lifecycle leases distinguish an attached prompt from an active agent run.
# No child-process or session-file heuristic: MCP servers persist while idle.
# Shared with piso-task-watch (down vs not-down).
pi_status() {
  python3 /usr/local/lib/piso-activity-state.py
}

# proj_ctx prints "repo · branch · commit" (or "" when not a git repo)
proj_ctx() {
  local toplevel=""
  toplevel=$(timeout 2 "${GIT[@]}" -C /workspace rev-parse --show-toplevel 2>/dev/null) || true
  if [ -z "$toplevel" ]; then
    printf '%s' "$(basename /workspace 2>/dev/null)"
    return
  fi
  local branch="" commit=""
  branch=$(timeout 2 "${GIT[@]}" -C "$toplevel" branch --show-current 2>/dev/null) || true
  commit=$(timeout 2 "${GIT[@]}" -C "$toplevel" rev-parse --short HEAD 2>/dev/null) || true
  local repo
  repo=$(basename "$toplevel")
  printf '%s' "$repo${branch:+ · $branch}${commit:+ @$commit}"
}

# Spool: shared buffer with piso-informant for events that could not reach
# the gateway right now. On the agent volume: writable + persistent in every
# worker (incl. the read-only-rootfs monitor). The informant appends, we
# drain here once the gateway is reachable. Bounded: newest SPOOL_MAX lines.
SPOOL="${PISO_SPOOL_FILE:-/root/.pi/agent/spool/activity.jsonl}"
SPOOL_MAX=200
DRAIN_CAP=25        # lines replayed per loop pass (bounds loop latency)

# build_body renders a fully-escaped JSON payload from kind + text.
build_body() {
  printf '{"worker":"%s","slug":"%s","kind":"%s","text":"%s"}' \
    "$(san "$worker")" "$(san "$slug")" "$1" "$(san "$2")"
}

# post_raw POSTs one fully-built payload; success iff the gateway accepted.
# POST_CODE is the status from the last attempt (000 on transport failure).
# A 403 identity mismatch is checkin-and-retried once inside the helper.
post_raw() {
  local body="$1"
  POST_CODE=$(piso_gateway_post "${gw}/api/v1/worker/activity" "$body" /tmp/piso-activity-notify.json)
  [ "$POST_CODE" = "200" ] || [ "$POST_CODE" = "201" ]
}

# spool_append buffers one payload under the shared lock (same file and lock
# the informant writes to). Drops oldest lines past SPOOL_MAX.
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

# drain_spool replays buffered events (informant spool + our own spooled
# notes) once the gateway accepts them. Snapshot + truncate happen together
# under the lock so concurrent appends are never clobbered; failed lines are
# re-queued for the next pass. Replays are at-most-once per pass.
drain_spool() {
  [ -f "$SPOOL" ] || return 0
  {
    if ! flock -w 3 9 2>/dev/null; then
      return 0
    fi
    cp "$SPOOL" "$SPOOL.drain" 2>/dev/null || return 0
    : >"$SPOOL"
  } 9>"$SPOOL.lock"
  [ -s "$SPOOL.drain" ] || { rm -f "$SPOOL.drain"; return 0; }
  local n=0 line
  while IFS= read -r line && [ "$n" -lt "$DRAIN_CAP" ]; do
    [ -n "$line" ] || continue
    if post_raw "$line"; then
      :
    elif [ "${POST_CODE:-}" = "400" ]; then
      echo "activity 400 (dropped, not retried)"
    else
      spool_append "$line" || true   # back to the spool; retried next pass
    fi
    n=$((n+1))
  done <"$SPOOL.drain"
  rm -f "$SPOOL.drain"
}

# post sends one activity; true iff accepted NOW. Durable kinds (notes, and
# the informant's semantic events via the spool) are buffered on failure so a
# gateway blip cannot lose them. Ephemeral "active" beats are never spooled:
# they are alive-signals and replaying stale ones would mislead the board.
post() {
  local kind="$1" text="$2" body
  body=$(build_body "$kind" "$text")
  if post_raw "$body"; then
    return 0
  fi
  if [ "${POST_CODE:-}" = "400" ]; then
    echo "activity 400 (not spooled)"
    return 1
  fi
  if [ "$kind" != "active" ]; then
    spool_append "$body" || true
  fi
  return 1
}

last_beat=0
last_state=""   # "up" | "down"
idle_since=0    # unix secs when this not-working stretch began; 0 = n/a
idle_posted=0   # 1 once an idle row has been posted for this stretch
while true; do
  drain_spool
  now=$(date +%s 2>/dev/null) || now=0
  status="$(pi_status)"
  state="down"
  if [ "$status" = "busy" ] || [ "$status" = "idle" ]; then
    state="up"
  fi

  if [ "$state" != "$last_state" ]; then
    if [ "$state" = "up" ]; then
      post "note" "session started" || true
    else
      post "note" "session ended" || true
    fi
    # Advance regardless of delivery: a failed note was spooled (retained), so
    # the boundary is recorded and the live loop must not re-fire it.
    last_state="$state"
    last_beat=0   # force a fresh active beat after a boundary
    idle_since=0
    idle_posted=0
  fi

  # Every beat is stored; readers merge the history into active spans. Only
  # a fresh lifecycle lease can keep a worker active. Missing/stale leases
  # fail quiet rather than reverting to the old process-tree heuristic.
  working=0
  if [ "$status" = "busy" ]; then working=1; fi
  if [ "$working" = "1" ]; then
    idle_since=0
    idle_posted=0
    if [ $((now - last_beat)) -ge "$BEAT_SECS" ]; then
      ctx="$(proj_ctx)"
      if post "active" "working on ${ctx:-unknown}"; then
        last_beat=$now
      fi
    fi
  elif [ "$state" = "up" ]; then
    # pi is attached and not working: start (or continue) the idle clock.
    # One idle row per stretch, after IDLE_SECS — the monitor pokes on this.
    if [ "$idle_since" -eq 0 ]; then
      idle_since=$now
    fi
    if [ "$idle_posted" -eq 0 ] && [ $((now - idle_since)) -ge "$IDLE_SECS" ]; then
      ctx="$(proj_ctx)"
      post "idle" "idle at prompt · ${ctx:-unknown}" || true
      idle_posted=1   # even on spool: do not re-fire this stretch
    fi
  fi

  sleep "$RESCAN_SECS"
done