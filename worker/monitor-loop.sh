#!/bin/bash
# piso-monitor-loop — the project-manager's wake loop (monitor role only).
#
# Runs in piso-worker-monitor. On each wake it asks pi to read the activity
# feed, judge per MONITOR.md, and emit any pokes/reminders/notes via
# piso-informant. The loop gives the cadence; pi's session (scoped to the
# monitor's own agent volume) keeps the judge's state across wakes.
#
# Design notes
# - pi -p runs a single-shot task and exits; the shell loop is the scheduler.
# - GATEWAY_URL points at the worker API (:8083). The feed is scrubbed; the
#   monitor never touches the control plane.
# - The monitor's model calls go through the MITM with its SCOPED key
#   (placeholder piso_monitor_…, Workers: [monitor]) — only it can substitute.
# - MONITOR.md ships at /opt/piso/MONITOR.md and is ALSO loaded via
#   settings.prompts; passing it explicitly here ensures the frame even before
#   pi's session has it in memory.
set -u

gw="${GATEWAY_URL:-}"
worker="${PISO_WORKER_NAME:-}"
slug="${PISO_WORKER_SLUG:-}"
if [ -z "$gw" ] || [ -z "$worker" ] || [ "$slug" != "monitor" ]; then
  echo "piso-monitor-loop: monitor role only (GATEWAY_URL, PISO_WORKER_NAME, slug=monitor)" >&2
  exit 2
fi

WAKE_SECS="${PISO_MONITOR_WAKE_SECS:-600}"   # 10 min default cadence
PROMPT="/opt/piso/MONITOR.md"

if [ ! -f "$PROMPT" ]; then
  echo "piso-monitor-loop: missing $PROMPT" >&2
  exit 2
fi
piBin="$(command -v pi 2>/dev/null || echo /usr/local/bin/pi)"

# The monitor has NO project mount and a read-only rootfs; the default workdir
# /workspace is unwritable, so pi's project-local extension state (cwd/.pi)
# fails to create. Use a persistent workdir inside the agent VOLUME (not
# tmpfs) so extension state AND pi's project identity survive restarts. The
# monitor's session lives here too (/root/.pi/agent/sessions/<cwd-key>).
MONITOR_HOME="/root/.pi/agent/monitor-work"
mkdir -p "$MONITOR_HOME" 2>/dev/null && cd "$MONITOR_HOME" 2>/dev/null || cd /tmp 2>/dev/null || true

while true; do
  echo "monitor wake $(date -u +%H:%M:%S)"
  # One decision pass. pi reads the feed per MONITOR.md and emits pokes via
  # piso-informant. Time-boxed: if the model call stalls, the loop still wakes
  # next time (piso can't interrupt a hung pi easily; an external guard would
  # need to kill the session — out of scope for v1).
  "$piBin" -p "Run your project-manager routine now: read the activity feed per $(basename $PROMPT) and emit any pokes/reminders/notes via piso-informant. Be conservative: usually the right call is NO new events." \
    2>&1 | tail -20
  sleep "$WAKE_SECS"
done