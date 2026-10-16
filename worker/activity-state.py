#!/usr/bin/env python3
"""Read pi lifecycle leases. Never infer work from processes or transcript mtimes.

A lease renews every 10s and expires after 45s. Validate boot ID and process
start time as well as PID: agent volumes survive container restarts/PID reuse.
Missing, corrupt, dead, or stale producers are not evidence of activity.
"""
import json
import os
from pathlib import Path

LEASE_SECS = 45


def activity_status(directory, proc=Path("/proc"), now=None):
    proc = Path(proc)
    try:
        boot_id = (proc / "sys/kernel/random/boot_id").read_text().strip()
        if now is None:
            now = float((proc / "uptime").read_text().split()[0])
    except (OSError, ValueError, IndexError):
        return "down"
    status = "down"
    for path in Path(directory).glob("*.json"):
        try:
            # Fixed-size metadata only; refuse unbounded/malformed input.
            with path.open() as handle:
                row = json.loads(handle.read(4096))
            if row.get("version") != 1 or row.get("state") not in ("busy", "idle"):
                continue
            pid = row.get("pid")
            if type(pid) is not int or pid <= 0:
                continue
            if row.get("bootId") != boot_id:
                path.unlink(missing_ok=True)
                continue
            try:
                stat = (proc / str(pid) / "stat").read_text()
            except FileNotFoundError:
                path.unlink(missing_ok=True)
                continue
            fields = stat[stat.rfind(")") + 2:].split()
            if fields[0] in ("Z", "X") or fields[19] != row.get("startTicks"):
                path.unlink(missing_ok=True)
                continue
            updated = row.get("updated")
            if type(updated) not in (int, float) or not 0 <= now - updated <= LEASE_SECS:
                continue
            if row["state"] == "busy":
                status = "busy"
            elif status != "busy":
                status = "idle"
        except (OSError, ValueError, TypeError, AttributeError, IndexError):
            continue
    return status


if __name__ == "__main__":
    print(activity_status(os.environ.get("PISO_ACTIVITY_DIR", "/root/.pi/agent/activity-state")))
