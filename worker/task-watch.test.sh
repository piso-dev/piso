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

U1=11111111-1111-1111-1111-111111111111
U2=22222222-2222-2222-2222-222222222222
U3=33333333-3333-3333-3333-333333333333
U4=44444444-4444-4444-4444-444444444444
U5=55555555-5555-5555-5555-555555555555
U6=66666666-6666-6666-6666-666666666666
U7=77777777-7777-7777-7777-777777777777
U8=88888888-8888-8888-8888-888888888888
U9=99999999-9999-9999-9999-999999999999
UA=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa
UB=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb
UC=cccccccc-cccc-cccc-cccc-cccccccccccc
UD=dddddddd-dddd-dddd-dddd-dddddddddddd
UE=eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee

held='{"status":"ok","tasks":[
  {"id":"'$U2'","resumable":false,"claimable":true,"priority":10},
  {"id":"'$U1'","resumable":true,"claimable":false,"priority":0}
]}'
got=$(printf '%s\n' "$held" | pick)
[ "$got" = "resume $U1" ] && ok "resumable beats claimable" || bad "resumable beats claimable (got $got)"

resume_prio='{"status":"ok","tasks":[
  {"id":"'$U4'","resumable":true,"claimable":false,"priority":5},
  {"id":"'$U3'","resumable":true,"claimable":false,"priority":9}
]}'
got=$(printf '%s\n' "$resume_prio" | pick)
[ "$got" = "resume $U3" ] && ok "resumable highest priority first" || bad "resumable priority (got $got)"

prio='{"status":"ok","tasks":[
  {"id":"'$U6'","claimable":true,"priority":1},
  {"id":"'$U5'","claimable":true,"priority":3}
]}'
got=$(printf '%s\n' "$prio" | pick)
[ "$got" = "start $U5" ] && ok "higher priority first" || bad "higher priority first (got $got)"

ids='{"status":"ok","tasks":[
  {"id":"'$U8'","claimable":true,"priority":1},
  {"id":"'$U7'","claimable":true,"priority":1}
]}'
seen=""
invalid=0
for _ in $(seq 1 40); do
  got=$(printf '%s\n' "$ids" | pick)
  case "$got" in
    "start $U7"|"start $U8") ;;
    *) invalid=1 ;;
  esac
  seen="$seen $got"
done
printf '%s' "$seen" | grep -q "start $U7" && printf '%s' "$seen" | grep -q "start $U8" \
  && ok "same priority → random spread" || bad "same priority random (got$seen)"
[ "$invalid" = "0" ] && ok "random pick stays a candidate" || bad "random pick invalid (got$seen)"

blocked='{"status":"ok","tasks":[
  {"id":"'$U3'","claimable":false,"priority":9}
]}'
got=$(printf '%s\n' "$blocked" | pick)
[ "$got" = "sleep" ] && ok "claimable false → sleep" || bad "claimable false → sleep (got $got)"

wrapped='{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"status\":\"ok\",\"tasks\":[{\"id\":\"'$U9'\",\"claimable\":true,\"priority\":0}]}"}]}}'
got=$(printf '%s\n' "$wrapped" | pick)
[ "$got" = "start $U9" ] && ok "unwrap MCP tools/call" || bad "unwrap MCP tools/call (got $got)"

# --- loop ---
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/bin"

cat >"$root/bin/piso-mcp" <<'EOF'
#!/bin/bash
set -u
args="$*"
if [[ "$args" == *task_claim* ]]; then
  printf '%s\n' "$args" >>"${CLAIM_LOG:-/dev/null}"
  if [ "${FAKE_CLAIM_RC:-0}" != "0" ]; then
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"conflict"}}'
    exit 0
  fi
  printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"id\":\"t\",\"status\":\"working\",\"attemptId\":\"'"${FAKE_ATTEMPT:-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}"'\"}"}]}}'
  exit 0
fi
if [[ "$args" == *task_renew* ]]; then
  printf '%s\n' "$args" >>"${RENEW_LOG:-/dev/null}"
  printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}'
  exit 0
fi
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
printf '%s\n' "PISO_TASK_ATTEMPT=${PISO_TASK_ATTEMPT-}" >>"${PI_LOG}"
printf '%s\n' "$*" >>"${PI_LOG}"
EOF
chmod +x "$root/bin/pi"

cat >"$root/bin/pi-slow" <<'EOF'
#!/bin/bash
printf '%s\n' "PISO_TASK_ATTEMPT=${PISO_TASK_ATTEMPT-}" >>"${PI_LOG}"
sleep 2
EOF
chmod +x "$root/bin/pi-slow"

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
    PISO_PI="${PISO_PI_BIN:-$root/bin/pi}" \
    PISO_TASK_PICK="$(pwd)/task-pick.py" \
    PISO_TASK_SESSION_DIR="$root/sessions" \
    PISO_PI_STATUS="$1" \
    PISO_TASK_IDLE_SECS=0 \
    PISO_TASK_IDLE_JITTER="${PISO_TASK_IDLE_JITTER-0}" \
    PISO_TASK_SCAN_SECS=0 \
    PISO_TASK_HEARTBEAT_SECS="${PISO_TASK_HEARTBEAT_SECS-0}" \
    PISO_TASK_WATCH_MAX="$2" \
    PI_LOG="$PI_LOG" \
    FAKE_MCP_OUT="${FAKE_MCP_OUT-}" \
    FAKE_MCP_RC="${FAKE_MCP_RC-0}" \
    FAKE_MCP_QUEUE="${FAKE_MCP_QUEUE-}" \
    FAKE_CLAIM_RC="${FAKE_CLAIM_RC-0}" \
    FAKE_ATTEMPT="${FAKE_ATTEMPT-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}" \
    CLAIM_LOG="${CLAIM_LOG-}" \
    RENEW_LOG="${RENEW_LOG-}" \
    bash ./task-watch.sh
}

PI_LOG="$root/pi.log"
CLAIM_LOG="$root/claim.log"
RENEW_LOG="$root/renew.log"
: >"$PI_LOG"
: >"$CLAIM_LOG"
: >"$RENEW_LOG"
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
FAKE_MCP_OUT='{"status":"ok","tasks":[{"id":"'$UA'","resumable":true,"claimable":false,"priority":0}]}'
out=$(run_watch "$root/bin/pi-status-down" 1)
printf '%s\n' "$out" | grep -q "resume $UA" && ok "loop resume" || bad "loop resume ($out)"
grep -q "PISO_TASK_ID=$UA" "$PI_LOG" && ok "resume exports PISO_TASK_ID" || bad "resume exports PISO_TASK_ID"
grep -q -- "--session $root/sessions/task-$UA.jsonl" "$PI_LOG" && ok "resume uses task session" || bad "resume uses task session ($(cat "$PI_LOG"))"

: >"$PI_LOG"
FAKE_MCP_OUT='{"status":"ok","tasks":[{"id":"'$UB'","resumable":false,"claimable":true,"priority":0}]}'
out=$(run_watch "$root/bin/pi-status-down" 1)
printf '%s\n' "$out" | grep -q "start $UB" && ok "loop start" || bad "loop start ($out)"
grep -q -- "--session $root/sessions/task-$UB.jsonl" "$PI_LOG" && ok "start uses task session" || bad "start uses task session"
grep -q "PISO_TASK_ATTEMPT=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" "$PI_LOG" && ok "start exports attemptId" || bad "start exports attemptId ($(cat "$PI_LOG"))"
grep -q "$UB" "${CLAIM_LOG:-/dev/null}" && ok "watch claims before pi" || bad "watch claims before pi"

# A lost race must not spend a pi session.
: >"$PI_LOG"
FAKE_MCP_OUT='{"status":"ok","tasks":[{"id":"'$UE'","resumable":false,"claimable":true,"priority":0}]}'
FAKE_CLAIM_RC=1
out=$(run_watch "$root/bin/pi-status-down" 1)
FAKE_CLAIM_RC=0
printf '%s\n' "$out" | grep -q "claim lost $UE" && ok "claim conflict → skip" || bad "claim conflict skip ($out)"
if [ -s "$PI_LOG" ]; then bad "claim conflict must not start pi"; else ok "claim conflict does not start pi"; fi

# The watcher heartbeats the lease while pi runs.
: >"$PI_LOG"
: >"$RENEW_LOG"
FAKE_MCP_OUT='{"status":"ok","tasks":[{"id":"'$UB'","resumable":false,"claimable":true,"priority":0}]}'
PISO_TASK_HEARTBEAT_SECS=1
PISO_PI_BIN="$root/bin/pi-slow"
out=$(run_watch "$root/bin/pi-status-down" 1)
PISO_PI_BIN=""
PISO_TASK_HEARTBEAT_SECS=0
grep -q "$UB" "$RENEW_LOG" && ok "watch heartbeats the lease" || bad "watch heartbeats the lease"

: >"$PI_LOG"
FAKE_MCP_OUT='{"status":"ok","tasks":[{"id":"'$U3'","resumable":false,"claimable":true,"priority":0}]}'
out=$(run_watch "$root/bin/pi-status-idle" 1)
printf '%s\n' "$out" | grep -q 'wait pi' && ok "pi running → wait" || bad "pi running → wait ($out)"
if [ -s "$PI_LOG" ]; then bad "wait pi must not start pi"; else ok "wait pi does not start pi"; fi

: >"$PI_LOG"
queue="$root/queue.jsonl"
printf '%s\n' \
  '{"status":"ok","tasks":[{"id":"'$UC'","resumable":false,"claimable":true,"priority":0}]}' \
  '{"status":"ok","tasks":[{"id":"'$UD'","resumable":false,"claimable":true,"priority":0}]}' \
  >"$queue"
FAKE_MCP_QUEUE="$queue"
FAKE_MCP_OUT=""
out=$(run_watch "$root/bin/pi-status-down" 2)
printf '%s\n' "$out" | grep -q "start $UC" && printf '%s\n' "$out" | grep -q "start $UD" && ok "chained tasks start without idle" || bad "chained tasks ($out)"
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
