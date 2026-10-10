#!/bin/bash
# piso-task-watch — unattended inbox loop (no LLM).
#
# Peek inbox_list over the sealed piso-mcp channel, pick one task with
# task-pick.py (resumable → resume, claimable → start, else sleep), run one
# pi session for that task id, never start a second pi.
#
# Enabled by the entrypoint when PISO_TASK_LOOP=1. Not a tool. Not a long-poll.
set -u

if [ "${PISO_ROLE:-}" = "monitor" ]; then
  echo "piso-task-watch: monitor role does not run the task loop" >&2
  exit 3
fi

worker="${PISO_WORKER_NAME:-}"
slug="${PISO_WORKER_SLUG:-}"
if [ -z "$worker" ] || [ -z "$slug" ]; then
  echo "piso-task-watch: PISO_WORKER_NAME/PISO_WORKER_SLUG must be set" >&2
  exit 3
fi

MCP="${PISO_MCP:-piso-mcp}"
PI_BIN="${PISO_PI:-}"
if [ -z "$PI_BIN" ]; then
  PI_BIN="$(command -v pi 2>/dev/null || echo /usr/local/bin/pi)"
fi
PICK="${PISO_TASK_PICK:-/usr/local/lib/piso-task-pick.py}"
PI_STATUS="${PISO_PI_STATUS:-/usr/local/lib/piso-activity-state.py}"
IDLE_SECS="${PISO_TASK_IDLE_SECS:-300}"
SCAN_SECS="${PISO_TASK_SCAN_SECS:-3}"
MAX="${PISO_TASK_WATCH_MAX:-0}"
SESSION_DIR="${PISO_TASK_SESSION_DIR:-/root/.pi/agent/task-sessions}"
mkdir -p "$SESSION_DIR"
export PISO_TASK_EXCLUDE=""

pi_status() {
  python3 "$PI_STATUS" 2>/dev/null | head -1
}

peek() {
  timeout 30 "$MCP" call tools/call '{"name":"inbox_list","arguments":{}}'
}

decide() {
  python3 "$PICK"
}

start_pi() {
  local mode="$1" id="$2"
  export PISO_TASK_ID="$id"
  echo "$mode $id"
  "$PI_BIN" --session "$SESSION_DIR/task-${id}.jsonl" -p "Unattended run for task ${id}. Read inbox_get for current feedback and checkpoint. Claim with a fresh requestId; keep the returned attemptId for all writes and renew the lease while working. A conflict means exit, not retry. Do not claim another task. Checkpoint and complete, fail with evidence, release, or task_wait, then exit. A PR normally means wait/review. Task-linked approval and human-action requests also wait atomically. Never sit at the prompt."
  # Each task gets at most one launch per sweep, even when several sessions
  # fail before changing state. New work can still run without a 300s delay.
  export PISO_TASK_EXCLUDE="${PISO_TASK_EXCLUDE:+${PISO_TASK_EXCLUDE},}${id}"
}

idle_sleep() {
  sleep "$IDLE_SECS"
  export PISO_TASK_EXCLUDE=""
}

error_sleep() {
  local jitter wait
  jitter=$((RANDOM % 3))
  wait=$((backoff + jitter))
  echo "piso-task-watch: peek failed (retry in ${wait}s)"
  sleep "$wait"
  if [ "$backoff" -lt 30 ]; then
    backoff=$((backoff * 2))
    if [ "$backoff" -gt 30 ]; then
      backoff=30
    fi
  fi
}

backoff=2
waiting=0
n=0
while true; do
  n=$((n + 1))
  st=$(pi_status)
  if [ "$st" != "down" ]; then
    if [ "$waiting" = "0" ]; then
      echo "wait pi"
      waiting=1
    fi
    sleep "$SCAN_SECS"
    if [ "$MAX" -gt 0 ] && [ "$n" -ge "$MAX" ]; then
      exit 0
    fi
    continue
  fi
  waiting=0

  raw=""
  if ! raw=$(peek 2>/dev/null); then
    error_sleep
    if [ "$MAX" -gt 0 ] && [ "$n" -ge "$MAX" ]; then
      exit 0
    fi
    continue
  fi
  backoff=2

  decision=$(printf '%s\n' "$raw" | decide)
  case "$decision" in
    resume\ *)
      start_pi resume "${decision#resume }"
      ;;
    start\ *)
      start_pi start "${decision#start }"
      ;;
    unpaired)
      echo "unpaired"
      idle_sleep
      ;;
    sleep)
      echo "peek empty"
      idle_sleep
      ;;
    *)
      echo "piso-task-watch: peek error"
      error_sleep
      ;;
  esac

  if [ "$MAX" -gt 0 ] && [ "$n" -ge "$MAX" ]; then
    exit 0
  fi
done
