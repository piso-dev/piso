#!/bin/bash
# piso-task-watch — unattended inbox loop (no LLM).
#
# Peek inbox_list over the sealed piso-mcp channel, pick one task with
# task-pick.py (resumable → resume, claimable → start, else sleep), claim it
# BEFORE spending a model session, then run one pi session for that task,
# renewing the lease with a heartbeat until pi exits. Never start a second pi.
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
# Jitter the idle wake so a fleet booted together does not stampede inbox_list.
IDLE_JITTER="${PISO_TASK_IDLE_JITTER:-60}"
SCAN_SECS="${PISO_TASK_SCAN_SECS:-3}"
HEARTBEAT_SECS="${PISO_TASK_HEARTBEAT_SECS:-60}"
MAX="${PISO_TASK_WATCH_MAX:-0}"
SESSION_DIR="${PISO_TASK_SESSION_DIR:-/root/.pi/agent/task-sessions}"
mkdir -p "$SESSION_DIR"
# peek() writes its stderr here so a failure can say why (see error_sleep).
PEEK_ERR="$SESSION_DIR/peek.err"
export PISO_TASK_EXCLUDE=""

pi_status() {
  python3 "$PI_STATUS" 2>/dev/null | head -1
}

peek() {
  timeout 30 "$MCP" call tools/call '{"name":"inbox_list","arguments":{}}'
}

# peek()'s stderr used to go to /dev/null, so a channel that could not start at
# all ("gateway pin missing", an expired key, a gateway 403) was indistinguishable
# from a flaky network: the log said `peek failed (retry in 30s)` forever and the
# reason had to be rediscovered by hand. Keep it in the log instead.
peek_reason() {
  local file="$1" line=""
  if [ -s "$file" ]; then
    line=$(head -c 400 "$file" | tr '\r\n' '  ' | sed 's/[[:space:]]*$//')
  fi
  printf '%s' "$line"
}

decide() {
  python3 "$PICK"
}

new_request_id() {
  python3 -c 'import uuid; print(uuid.uuid4())'
}

# Print attemptId from a tools/call response, or exit nonzero on a conflict.
extract_attempt() {
  python3 -c '
import json
import sys


def unwrap(doc):
    if not isinstance(doc, dict):
        return None
    if "result" in doc and isinstance(doc.get("result"), dict):
        doc = doc.get("result")
    if doc.get("isError") is True:
        return None
    if "content" in doc and "attemptId" not in doc:
        parts = []
        for item in doc.get("content") or []:
            if isinstance(item, dict) and item.get("type") == "text":
                parts.append(item.get("text") or "")
        blob = "".join(parts).strip()
        if not blob:
            return None
        try:
            doc = json.loads(blob)
        except ValueError:
            return None
    return doc


try:
    doc = unwrap(json.load(sys.stdin))
except ValueError:
    doc = None
if not isinstance(doc, dict) or not doc.get("attemptId"):
    sys.exit(1)
print(doc["attemptId"])
'
}

# Take the lease before launching pi. In a multi-worker race the loser pays one
# cheap HTTP call instead of a full model boot; TASKLOOP says a conflict exits.
claim_attempt() {
  local id="$1" raw
  if ! raw=$(timeout 30 "$MCP" call tools/call \
      "{\"name\":\"task_claim\",\"arguments\":{\"id\":\"${id}\",\"requestId\":\"$(new_request_id)\"}}" 2>/dev/null); then
    return 1
  fi
  printf '%s\n' "$raw" | extract_attempt
}

# Renew the lease while pi runs. The default lease is short, so a crashed watcher
# frees the task in minutes rather than half an hour.
heartbeat() {
  local id="$1" attempt="$2"
  while :; do
    sleep "$HEARTBEAT_SECS"
    timeout 30 "$MCP" call tools/call \
      "{\"name\":\"task_renew\",\"arguments\":{\"id\":\"${id}\",\"attemptId\":\"${attempt}\",\"requestId\":\"$(new_request_id)\"}}" \
      >/dev/null 2>&1 || true
  done
}

start_pi() {
  local mode="$1" id="$2" attempt="$3" hb=""
  export PISO_TASK_ID="$id"
  export PISO_TASK_ATTEMPT="$attempt"
  echo "$mode $id"
  if [ "$HEARTBEAT_SECS" -gt 0 ]; then
    heartbeat "$id" "$attempt" &
    hb=$!
  fi
  "$PI_BIN" --session "$SESSION_DIR/task-${id}.jsonl" -p "Unattended run for task ${id}. The watcher already claimed it and exported PISO_TASK_ATTEMPT=${attempt}; read inbox_get for current feedback and checkpoint, then use that attemptId for all writes and renew the lease while working. A conflict means exit, not retry. Do not claim another task. Checkpoint and complete, fail with evidence, release, or task_wait, then exit. A PR normally means wait/review. Task-linked approval and human-action requests also wait atomically. Never sit at the prompt."
  if [ -n "$hb" ]; then
    kill "$hb" 2>/dev/null
    wait "$hb" 2>/dev/null
  fi
  unset PISO_TASK_ATTEMPT
  # Each task gets at most one launch per sweep, even when several sessions
  # fail before changing state. New work can still run without a 300s delay.
  export PISO_TASK_EXCLUDE="${PISO_TASK_EXCLUDE:+${PISO_TASK_EXCLUDE},}${id}"
}

# A claim that loses the race is excluded for this sweep so the loop moves on.
skip_lost() {
  local id="$1"
  echo "claim lost $id"
  export PISO_TASK_EXCLUDE="${PISO_TASK_EXCLUDE:+${PISO_TASK_EXCLUDE},}${id}"
}

idle_sleep() {
  local jitter=0
  if [ "$IDLE_JITTER" -gt 0 ]; then
    jitter=$((RANDOM % IDLE_JITTER))
  fi
  sleep $((IDLE_SECS + jitter))
  export PISO_TASK_EXCLUDE=""
}

error_sleep() {
  local jitter wait reason="${1:-}"
  jitter=$((RANDOM % 3))
  wait=$((backoff + jitter))
  if [ -n "$reason" ]; then
    echo "piso-task-watch: peek failed (retry in ${wait}s): $reason"
  else
    echo "piso-task-watch: peek failed (retry in ${wait}s)"
  fi
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
  if ! raw=$(peek 2>"$PEEK_ERR"); then
    error_sleep "$(peek_reason "$PEEK_ERR")"
    if [ "$MAX" -gt 0 ] && [ "$n" -ge "$MAX" ]; then
      exit 0
    fi
    continue
  fi
  backoff=2

  decision=$(printf '%s\n' "$raw" | decide)
  case "$decision" in
    resume\ *)
      id="${decision#resume }"
      if attempt=$(claim_attempt "$id"); then
        start_pi resume "$id" "$attempt"
      else
        skip_lost "$id"
      fi
      ;;
    start\ *)
      id="${decision#start }"
      if attempt=$(claim_attempt "$id"); then
        start_pi start "$id" "$attempt"
      else
        skip_lost "$id"
      fi
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
      echo "piso-task-watch: peek error: $(printf '%s' "$raw" | head -c 300 | tr '\r\n' '  ')"
      error_sleep
      ;;
  esac

  if [ "$MAX" -gt 0 ] && [ "$n" -ge "$MAX" ]; then
    exit 0
  fi
done
