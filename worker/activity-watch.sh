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
#   start/end). A gap of > 3 min between active beats is downtime (the board
#   will not extend the block). Idle-at-prompt is not active; `waiting` is
#   Tier B only (the agent emits it via piso-informant).
# - Sanitization identical to context-watch (control chars → space, cap len).
set -u

gw="${GATEWAY_URL:-}"
worker="${PISO_WORKER_NAME:-}"
slug="${PISO_WORKER_SLUG:-}"

if [ -z "$gw" ] || [ -z "$worker" ] || [ -z "$slug" ]; then
  exit 0
fi

BEAT_SECS=60             # active beat at most every 1 min (board merges ≤ 3 min)
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

# pi_status prints down | busy | idle
#   down: no pi agent process
#   busy: pi is running and has descendant processes (a tool/command in flight)
#   idle: pi is up but has no tool children (typically sitting at the prompt)
pi_status() {
  python3 - <<'PY' 2>/dev/null | head -1
import os, re
want1 = re.compile(r'pi-coding-agent')
want2 = re.compile(r'(^|\s)pi(\s|$)')
bad = re.compile(r'piso-entrypoint|piso-planning-watch|piso-context-watch|piso-ports-watch|piso-activity-watch|piso-monitor-loop|docker-init|sleep infinity')

def cmdline(pid):
    raw = open('/proc/%s/cmdline' % pid, 'rb').read().replace(b'\0', b' ')
    return raw.decode('utf-8', 'replace')

def is_pi(cmd):
    if bad.search(cmd): return False
    return bool(want1.search(cmd) or want2.search(cmd))

pi_pids = []
for p in os.listdir('/proc'):
    if not p.isdigit(): continue
    try:
        cmd = cmdline(p)
        if is_pi(cmd):
            pi_pids.append(int(p))
    except Exception:
        pass
if not pi_pids:
    print('down')
    raise SystemExit

def children_of(pid):
    kids = set()
    task = '/proc/%d/task' % pid
    try:
        for tid in os.listdir(task):
            try:
                raw = open('%s/%s/children' % (task, tid)).read().split()
                kids.update(int(x) for x in raw)
            except Exception:
                pass
    except Exception:
        pass
    return kids

ppid_of = {}
for p in os.listdir('/proc'):
    if not p.isdigit(): continue
    try:
        st = open('/proc/%s/stat' % p).read()
        rp = st.rfind(')')
        fields = st[rp+2:].split()
        ppid_of[int(p)] = int(fields[1])
    except Exception:
        pass

seen = set(pi_pids)
stack = list(pi_pids)
desc = set()
while stack:
    pid = stack.pop()
    for k in children_of(pid):
        if k not in seen:
            seen.add(k); stack.append(k); desc.add(k)
    for c, parent in list(ppid_of.items()):
        if parent == pid and c not in seen:
            seen.add(c); stack.append(c); desc.add(c)
print('busy' if desc else 'idle')
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
  status="$(pi_status)"
  state="down"
  if [ "$status" = "busy" ] || [ "$status" = "idle" ]; then
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

  # alive beat — the board's live marker. Fire when there is recent session-file
  # activity (model/tool exchange) OR a tool child is still running, so an idle
  # attached pi does not keep the board "live".
  recent=$(session_recency); recent=${recent:-0}
  jsonl_fresh=0
  if [ "$recent" -ge $((now - 20)) ]; then jsonl_fresh=1; fi
  working=0
  if [ "$state" = "up" ] && { [ "$jsonl_fresh" = "1" ] || [ "$status" = "busy" ]; }; then
    working=1
  fi
  if [ "$working" = "1" ] && [ $((now - last_beat)) -ge "$BEAT_SECS" ]; then
    ctx="$(proj_ctx)"
    if post "active" "working on ${ctx:-unknown}"; then
      last_beat=$now
    fi
  fi

  sleep "$RESCAN_SECS"
done