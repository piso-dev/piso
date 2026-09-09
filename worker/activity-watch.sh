#!/bin/bash
# Tier A activity informant: derive COARSE activity from the worker without the
# agent's help, and post it to the gateway's activity store. The PM board's
# "floor" (alive / idle / session start) comes from here; the semantic richness
# (progress / milestones / pokes) comes from Tier B (the agent itself).
#
# Design notes
# - Same pattern as context-watch / ports-watch: in-container loop, POST to the
#   worker API (GATEWAY_URL, :8083) — never the host control plane.
# - Events: kind=active (alive beat w/ project context), kind=note (session
#   start/end). Idle is DERIVED gateway-side (no active beats for N min), so we
#   do not emit a separate idle event.
# - Sanitization identical to context-watch (control chars → space, cap len).
set -u

gw="${GATEWAY_URL:-}"
worker="${PISO_WORKER_NAME:-}"
slug="${PISO_WORKER_SLUG:-}"

if [ -z "$gw" ] || [ -z "$worker" ] || [ -z "$slug" ]; then
  exit 0
fi

BEAT_SECS=120            # post an active beat at most every 2 min
RESCAN_SECS=5            # check for session boundary every 5 s
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

# json_bool outputs true/false for a pi-process-present check
pi_present() {
  python3 - <<'PY' 2>/dev/null | head -1
import os, re
want1 = re.compile(r'pi-coding-agent')
want2 = re.compile(r'(^|\s)pi(\s|$)')
bad = re.compile(r'piso-entrypoint|piso-planning-watch|piso-context-watch|piso-ports-watch|piso-activity-watch|docker-init|sleep infinity')
for p in os.listdir('/proc'):
    if not p.isdigit(): continue
    try:
        raw = open('/proc/%s/cmdline' % p, 'rb').read().replace(b'\0', b' ')
        cmd = raw.decode('utf-8', 'replace')
        if bad.search(cmd): continue
        if want1.search(cmd) or want2.search(cmd):
            print('1'); break
    except Exception:
        pass
PY
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

# post sends one activity; success iff the gateway accepted (200/201)
post() {
  local kind="$1" text="$2"
  local code
  code=$(curl -sS --max-time 3 -o /tmp/piso-activity-notify.json -w '%{http_code}' \
    -X POST "${gw}/api/v1/worker/activity" \
    -H 'Content-Type: application/json' \
    -d "{\"worker\":\"$(san "$worker")\",\"slug\":\"$(san "$slug")\",\"kind\":\"$kind\",\"text\":\"$(san "$text")\"}" || true)
  [ "${code}" = "200" ] || [ "${code}" = "201" ]
}

# session_recency outputs the newest mtime (unix secs) of any pi session file,
# or 0 when none exists. pi APPENDS to the session jsonl as it exchanges with
# the model/tools, so a recent mtime means REAL activity; a stale mtime means
# an idle pi process (e.g. an attach sitting at a prompt) — which is NOT work.
session_recency() {
  python3 - <<'PY' 2>/dev/null | head -1
import glob, os
paths = glob.glob('/root/.pi/agent/sessions/**/*.jsonl', recursive=True)
best = 0
for f in paths:
    try:
        m = os.path.getmtime(f)
        if m > best: best = m
    except Exception:
        pass
print(int(best))
PY
}

last_beat=0
last_state=""   # "up" | "down"
while true; do
  now=$(date +%s 2>/dev/null) || now=0
  state="down"
  if [ "$(pi_present)" = "1" ]; then
    state="up"
  fi

  if [ "$state" != "$last_state" ]; then
    if [ "$state" = "up" ]; then
      if post "note" "session started"; then last_state="$state"; fi
    else
      if post "note" "session ended"; then last_state="$state"; fi
    fi
    last_beat=0   # force a fresh active beat after a boundary
  fi

  # alive beat — the board's live marker. Fire ONLY when there is recent
  # session-file activity (real model/tool exchange), so an idle attached pi
  # does not keep the board "live".
  recent=$(session_recency)
  if [ "$recent" -ge $((now - BEAT_SECS)) ] && [ $((now - last_beat)) -ge "$BEAT_SECS" ]; then
    ctx="$(proj_ctx)"
    if post "active" "working on ${ctx:-unknown}"; then
      last_beat=$now
    fi
  fi

  sleep "$RESCAN_SECS"
done