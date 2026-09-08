#!/bin/bash
# Report the worker's live context to the gateway so the dashboard request
# log can label each row with: folder/path, git repo (project), git branch,
# git commit, and the pi model name.
#
# Design notes
# - Same pattern as piso-planning-watch: in-container loop, POST on change,
#   talks to the worker API (GATEWAY_URL, :8083), never the host control
#   plane or the MITM proxy. NO_PROXY covers "gateway" so curl goes direct.
# - Labels describe the worker at that moment (advisory only). Snapshotting
#   happens gateway-side in AppendLog so each log row is stable.
# - The image has no ps/pgrep; live pi processes are found with a python3
#   /proc cmdline scan (same approach as piso's CLI sessions.go).
# - The host-owned /workspace mount trips git's dubious-ownership check when
#   read by container root, hence `-c safe.directory=*` on every git call.
set -u

gw="${GATEWAY_URL:-}"
worker="${PISO_WORKER_NAME:-}"
slug="${PISO_WORKER_SLUG:-}"

if [ -z "$gw" ] || [ -z "$worker" ] || [ -z "$slug" ]; then
  exit 0
fi

# pi sanitizes the session cwd: /workspace → --workspace--
SESS_DIR="/root/.pi/agent/sessions/--workspace--"
GIT=(git -c safe.directory=*)

# repo_name_from_url prints the repository name from a git remote URL:
#   https://host/org/repo.git   git@host:org/repo    ssh://.../org/repo
#   file:///path/repo          /local/path
# Empty input (or a URL with no usable name) prints nothing, so callers can
# fall back to the toplevel basename.
repo_name_from_url() {
  local u="$1"
  u="${u#file://}"        # file:///path -> /path
  while [ "${u%/}" != "$u" ]; do u="${u%/}"; done  # strip trailing slashes
  u="${u%.git}"           # strip trailing .git
  printf '%s' "${u##*/}"   # last /-separated component (scp form: host:org/repo)
}

# san strips control characters, JSON-escapes, and caps length (labels end
# up rendered in HTML). Order matters: control chars first, then backslash,
# then double-quote.
san() {
  local v="$1" max="${2:-120}"
  v=$(printf '%s' "$v" | tr '\r\n\t' '   ')
  v="${v//\\/\\\\}"
  v="${v//\"/\\\"}"
  if [ "${#v}" -gt "$max" ]; then
    v="${v:0:$max}"
  fi
  printf '%s' "$v"
}

# detect_pid prints one live pi pid (any; all share cwd=/workspace).
detect_pid() {
  python3 - <<'PY' 2>/dev/null | head -1
import os, re, sys
want1 = re.compile(r'pi-coding-agent')
want2 = re.compile(r'(^|\s)pi(\s|$)')   # token exactly 'pi'
bad = re.compile(r'piso-entrypoint|piso-planning-watch|piso-context-watch|docker-init|sleep infinity')
for p in os.listdir('/proc'):
    if not p.isdigit():
        continue
    try:
        raw = open('/proc/%s/cmdline' % p, 'rb').read().replace(b'\0', b' ')
        cmd = raw.decode('utf-8', 'replace')
        if bad.search(cmd):
            continue
        if want1.search(cmd) or want2.search(cmd):
            print(p)
    except Exception:
        pass
PY
}

# model_ctx prints "provider/modelId" from the newest session file (last
# model_change line), falling back to the live process env. The session file
# grows large (MBs), so search from the END with a doubling window — the
# common case (model set recently) costs one 64KB read.
model_ctx() {
  local pid="$1" prov="" mid=""
  local latest=""
  latest=$(ls -t "${SESS_DIR}"/*.jsonl 2>/dev/null | head -1)
  if [ -n "$latest" ] && [ -f "$latest" ]; then
    local size win line
    size=$(wc -c < "$latest" 2>/dev/null) || size=0
    win=65536
    while :; do
      line=$(tail -c "$win" "$latest" 2>/dev/null | grep '"type":"model_change"' | tail -1)
      if [ -n "$line" ] || [ "$win" -ge "$size" ]; then
        break
      fi
      win=$((win * 4))
    done
    if [ -n "$line" ]; then
      prov=$(printf '%s' "$line" | grep -o '"provider":"[^"]*"' | head -1 | cut -d'"' -f4)
      mid=$(printf '%s' "$line" | grep -o '"modelId":"[^"]*"' | head -1 | cut -d'"' -f4)
    fi
  fi
  if { [ -z "$prov" ] || [ -z "$mid" ]; } && [ -n "$pid" ] && [ -r "/proc/${pid}/environ" ]; then
    local eprov emid
    eprov=$(tr '\0' '\n' < "/proc/${pid}/environ" 2>/dev/null | grep '^PI_PROVIDER=' | cut -d= -f2- | tail -1)
    emid=$(tr '\0' '\n' < "/proc/${pid}/environ" 2>/dev/null | grep '^PI_MODEL=' | cut -d= -f2- | tail -1)
    if [ -n "$eprov" ] && [ -n "$emid" ]; then
      prov="$eprov"; mid="$emid"
    fi
  fi
  if [ -n "$prov" ] && [ -n "$mid" ]; then
    printf '%s/%s' "$prov" "$mid"
  fi
}

# ctx computes the full context tuple, setting globals folder/project/branch/
# commit/model. Falls back to the /workspace mount when pi is not running.
ctx() {
  local pid="" toplevel=""
  pid=$(detect_pid)
  folder="/workspace"
  if [ -n "$pid" ]; then
    local f
    f=$(readlink -f "/proc/${pid}/cwd" 2>/dev/null) || true
    if [ -n "$f" ]; then
      folder="$f"
    fi
  fi
  project=""; branch=""; commit=""
  toplevel=$(timeout 3 "${GIT[@]}" -C "$folder" rev-parse --show-toplevel 2>/dev/null) || true
  if [ -n "$toplevel" ]; then
    # Prefer the repo name from the remote URL in .git/config; the toplevel
    # basename is a poor label because the worker always mounts the project
    # at the same path (e.g. /workspace).
    url=$(timeout 3 "${GIT[@]}" -C "$folder" remote get-url origin 2>/dev/null) || true
    if [ -z "$url" ]; then
      # No origin remote; fall back to the first configured remote, if any.
      first=$(timeout 3 "${GIT[@]}" -C "$folder" remote 2>/dev/null | head -1) || true
      if [ -n "$first" ]; then
        url=$(timeout 3 "${GIT[@]}" -C "$folder" remote get-url "$first" 2>/dev/null) || true
      fi
    fi
    if [ -n "$url" ]; then
      project=$(repo_name_from_url "$url")
    fi
    if [ -z "$project" ]; then
      project=$(basename "$toplevel")
    fi
    branch=$(timeout 3 "${GIT[@]}" -C "$folder" branch --show-current 2>/dev/null) || true
    commit=$(timeout 3 "${GIT[@]}" -C "$folder" rev-parse --short HEAD 2>/dev/null) || true
  fi
  if [ -z "$project" ]; then
    project=$(basename "$folder")
  fi
  model=$(model_ctx "$pid")
}

# notify POSTs the context; success iff the gateway answered 200/201.
notify() {
  local payload="$1"
  local code
  code=$(curl -sS --max-time 3 -o /tmp/piso-context-notify.json -w '%{http_code}' \
    -X POST "${gw}/api/v1/worker/context" \
    -H 'Content-Type: application/json' \
    -d "$payload" || true)
  echo "notify ${code} $(tr -d '\n' < /tmp/piso-context-notify.json 2>/dev/null)"
  [ "${code}" = "200" ] || [ "${code}" = "201" ]
}

last=""
while true; do
  ctx
  fp="${folder}|${project}|${branch}|${commit}|${model}"
  if [ "$fp" != "$last" ]; then
    payload=$(printf '{"worker":"%s","slug":"%s","folder":"%s","project":"%s","branch":"%s","commit":"%s","model":"%s"}' \
      "$(san "$worker")" "$(san "$slug")" "$(san "$folder")" \
      "$(san "$project")" "$(san "$branch")" "$(san "$commit")" "$(san "$model")")
    if notify "$payload"; then
      last="$fp"
    fi
  fi
  sleep 1
done