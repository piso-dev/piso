#!/bin/bash
# Mechanical picker + task-watch loop. Fake piso-mcp and pi. No gateway.
set -u
cd "$(dirname "$0")"

fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }

pick() {
  python3 ./task-pick.py
}

raw_peek='{"status":"ok","tasks":[]}'
got=$(printf '%s\n' "$raw_peek" | pick)
[ "$got" = "sleep" ] && ok "empty tasks → sleep" || bad "empty tasks → sleep (got $got)"

got=$(printf '%s\n' '{"status":"unpaired"}' | pick)
[ "$got" = "unpaired" ] && ok "unpaired status" || bad "unpaired status (got $got)"

got=$(printf '%s\n' '{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"unpaired"}}' | pick)
[ "$got" = "unpaired" ] && ok "jsonrpc unpaired" || bad "jsonrpc unpaired (got $got)"

got=$(printf '%s\n' '{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"unknown tool"}}' | pick)
[ "$got" = "error" ] && ok "unknown tool → error" || bad "unknown tool → error (got $got)"

held='{"status":"ok","tasks":[
  {"id":"aaa","heldByMe":false,"assignedToMe":true,"claimable":true,"priority":10},
  {"id":"bbb","heldByMe":true,"assignedToMe":true,"claimable":false,"priority":0}
]}'
got=$(printf '%s\n' "$held" | pick)
[ "$got" = "resume bbb" ] && ok "heldByMe beats claimable" || bad "heldByMe beats claimable (got $got)"

assigned='{"status":"ok","tasks":[
  {"id":"low","heldByMe":false,"assignedToMe":false,"claimable":true,"priority":5},
  {"id":"mine","heldByMe":false,"assignedToMe":true,"claimable":true,"priority":0}
]}'
got=$(printf '%s\n' "$assigned" | pick)
[ "$got" = "start mine" ] && ok "assignedToMe beats higher-priority unassigned" || bad "assignedToMe beats (got $got)"

prio='{"status":"ok","tasks":[
  {"id":"a","heldByMe":false,"assignedToMe":false,"claimable":true,"priority":1},
  {"id":"b","heldByMe":false,"assignedToMe":false,"claimable":true,"priority":3}
]}'
got=$(printf '%s\n' "$prio" | pick)
[ "$got" = "start b" ] && ok "higher priority first" || bad "higher priority first (got $got)"

ids='{"status":"ok","tasks":[
  {"id":"m","heldByMe":false,"assignedToMe":false,"claimable":true,"priority":1},
  {"id":"k","heldByMe":false,"assignedToMe":false,"claimable":true,"priority":1}
]}'
got=$(printf '%s\n' "$ids" | pick)
[ "$got" = "start k" ] && ok "same priority → id asc" || bad "id asc (got $got)"

blocked='{"status":"ok","tasks":[
  {"id":"nope","heldByMe":false,"assignedToMe":true,"claimable":false,"dependenciesMet":false,"directiveActive":true,"priority":9}
]}'
got=$(printf '%s\n' "$blocked" | pick)
[ "$got" = "sleep" ] && ok "claimable false → sleep" || bad "claimable false → sleep (got $got)"

wrapped='{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"status\":\"ok\",\"tasks\":[{\"id\":\"t1\",\"heldByMe\":false,\"assignedToMe\":true,\"claimable\":true,\"priority\":0}]}"}]}}'
got=$(printf '%s\n' "$wrapped" | pick)
[ "$got" = "start t1" ] && ok "unwrap MCP tools/call" || bad "unwrap MCP tools/call (got $got)"

# --- loop ---
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/bin"

cat >"$root/bin/piso-mcp" <<'EOF'
#!/bin/bash
set -u
queue="${FAKE_MCP_QUEUE:-}"
if [ -n "$queue" ] && [ -f "$queue" ]; then
  line=$(head -1 "$queue")
  tail -n +2 "$queue" >"$queue.tmp" && mv "$queue.tmp" "$queue"
  if [ -n "$line" ]; then
    printf '%s\n' "$line"
    exit 0
  fi
fi
if [ "${FAKE_MCP_RC:-0}" != "0" ]; then
  exit "$FAKE_MCP_RC"
fi
printf '%s\n' "${FAKE_MCP_OUT-}"
EOF
chmod +x "$root/bin/piso-mcp"

cat >"$root/bin/pi" <<'EOF'
#!/bin/bash
printf '%s\n' "PISO_TASK_ID=${PISO_TASK_ID-}" >>"${PI_LOG}"
printf '%s\n' "$*" >>"${PI_LOG}"
EOF
chmod +x "$root/bin/pi"

cat >"$root/bin/pi-status-down" <<'EOF'
#!/usr/bin/env python3
print("down")
EOF
chmod +x "$root/bin/pi-status-down"

cat >"$root/bin/pi-status-idle" <<'EOF'
#!/usr/bin/env python3
print("idle")
EOF
chmod +x "$root/bin/pi-status-idle"

run_watch() {
  env -i \
    PATH="$root/bin:/usr/bin:/bin" \
    PISO_WORKER_NAME="piso-worker-piso" \
    PISO_WORKER_SLUG="piso" \
    PISO_MCP="$root/bin/piso-mcp" \
    PISO_PI="$root/bin/pi" \
    PISO_TASK_PICK="$(pwd)/task-pick.py" \
    PISO_PI_STATUS="$1" \
    PISO_TASK_IDLE_SECS=0 \
    PISO_TASK_SCAN_SECS=0 \
    PISO_TASK_WATCH_MAX="$2" \
    PI_LOG="$PI_LOG" \
    FAKE_MCP_OUT="${FAKE_MCP_OUT-}" \
    FAKE_MCP_RC="${FAKE_MCP_RC-0}" \
    FAKE_MCP_QUEUE="${FAKE_MCP_QUEUE-}" \
    bash ./task-watch.sh
}

PI_LOG="$root/pi.log"
: >"$PI_LOG"
FAKE_MCP_OUT='{"status":"ok","tasks":[]}'
out=$(run_watch "$root/bin/pi-status-down" 1)
printf '%s\n' "$out" | grep -q 'peek empty' && ok "loop empty → peek empty" || bad "loop empty → peek empty ($out)"
if [ -s "$PI_LOG" ]; then bad "loop empty must not start pi"; else ok "loop empty does not start pi"; fi

: >"$PI_LOG"
FAKE_MCP_OUT='{"status":"unpaired"}'
out=$(run_watch "$root/bin/pi-status-down" 1)
printf '%s\n' "$out" | grep -q '^unpaired$' && ok "loop unpaired" || bad "loop unpaired ($out)"
if [ -s "$PI_LOG" ]; then bad "unpaired must not start pi"; else ok "unpaired does not start pi"; fi

: >"$PI_LOG"
FAKE_MCP_OUT='{"status":"ok","tasks":[{"id":"held-1","heldByMe":true,"assignedToMe":true,"claimable":false,"priority":0}]}'
out=$(run_watch "$root/bin/pi-status-down" 1)
printf '%s\n' "$out" | grep -q 'resume held-1' && ok "loop resume" || bad "loop resume ($out)"
grep -q 'PISO_TASK_ID=held-1' "$PI_LOG" && ok "resume exports PISO_TASK_ID" || bad "resume exports PISO_TASK_ID"
grep -q -- '--session task-held-1' "$PI_LOG" && ok "resume uses task session" || bad "resume uses task session ($(cat "$PI_LOG"))"

: >"$PI_LOG"
FAKE_MCP_OUT='{"status":"ok","tasks":[{"id":"new-1","heldByMe":false,"assignedToMe":true,"claimable":true,"priority":0}]}'
out=$(run_watch "$root/bin/pi-status-down" 1)
printf '%s\n' "$out" | grep -q 'start new-1' && ok "loop start" || bad "loop start ($out)"
grep -q -- '--session task-new-1' "$PI_LOG" && ok "start uses task session" || bad "start uses task session"

: >"$PI_LOG"
FAKE_MCP_OUT='{"status":"ok","tasks":[{"id":"should-not","heldByMe":false,"assignedToMe":true,"claimable":true,"priority":0}]}'
out=$(run_watch "$root/bin/pi-status-idle" 1)
printf '%s\n' "$out" | grep -q 'wait pi' && ok "pi running → wait" || bad "pi running → wait ($out)"
if [ -s "$PI_LOG" ]; then bad "wait pi must not start pi"; else ok "wait pi does not start pi"; fi

: >"$PI_LOG"
queue="$root/queue.jsonl"
printf '%s\n' \
  '{"status":"ok","tasks":[{"id":"one","heldByMe":false,"assignedToMe":true,"claimable":true,"priority":0}]}' \
  '{"status":"ok","tasks":[{"id":"two","heldByMe":false,"assignedToMe":true,"claimable":true,"priority":0}]}' \
  >"$queue"
FAKE_MCP_QUEUE="$queue"
FAKE_MCP_OUT=""
out=$(run_watch "$root/bin/pi-status-down" 2)
printf '%s\n' "$out" | grep -q 'start one' && printf '%s\n' "$out" | grep -q 'start two' && ok "chained tasks start without idle" || bad "chained tasks ($out)"
printf '%s\n' "$out" | grep -q 'peek empty' && bad "chained tasks slept" || ok "chained tasks did not sleep"
unset FAKE_MCP_QUEUE

out=$(env -i PATH="/usr/bin:/bin" PISO_ROLE=monitor PISO_WORKER_NAME=x PISO_WORKER_SLUG=monitor bash ./task-watch.sh 2>&1)
rc=$?
[ "$rc" = "3" ] && ok "monitor exits 3" || bad "monitor exits 3 (rc $rc)"

ep=$(cat ./entrypoint.sh)
printf '%s\n' "$ep" | grep -q 'PISO_TASK_LOOP' && printf '%s\n' "$ep" | grep -q 'piso-task-watch' && ok "entrypoint respawns watcher" || bad "entrypoint respawns watcher"
printf '%s\n' "$ep" | grep -q 'TASKLOOP.md' && ok "entrypoint folds TASKLOOP.md" || bad "entrypoint folds TASKLOOP.md"
grep -q 'piso-task-watch' ./Dockerfile && ok "image installs piso-task-watch" || bad "image installs piso-task-watch"
grep -q 'PISO_TASK_LOOP' ../compose/worker.yaml.tmpl && bad "local module must not set PISO_TASK_LOOP" || ok "local module does not set PISO_TASK_LOOP"

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "all ok"
