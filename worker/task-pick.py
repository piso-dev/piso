#!/usr/bin/env python3
"""Mechanical inbox picker for piso-task-watch.

Reads one JSON document from stdin (raw peek, MCP tools/call result, or
JSON-RPC envelope). Prints a single line:

  resume <id>
  start <id>
  sleep
  unpaired
  error

Never reads title, body, plan, or repo. resumable / claimable are computed
by the coordinator. Lease ownership alone does not make a task runnable.
"""
from __future__ import print_function

import json
import os
import random
import sys
import uuid


def _priority(task):
    try:
        return int(task.get("priority") or 0)
    except (TypeError, ValueError):
        return 0


def _choose(rows):
    """Highest priority wins; ties are broken at random so that N idle
    workers spread across N tasks instead of stampeding the same head task."""
    best = max(_priority(task) for task in rows)
    top = [_id(task) for task in rows if _priority(task) == best]
    return random.choice(top)


def _id(task):
    value = task.get("id")
    if value is None:
        return ""
    try:
        return str(uuid.UUID(str(value)))
    except (ValueError, TypeError, AttributeError):
        return ""


def unwrap(doc):
    if isinstance(doc, str):
        try:
            doc = json.loads(doc)
        except ValueError:
            return {"status": "error", "tasks": []}
    if not isinstance(doc, dict):
        return {"status": "error", "tasks": []}
    if "jsonrpc" in doc and doc.get("error"):
        msg = ""
        err = doc.get("error")
        if isinstance(err, dict):
            msg = str(err.get("message") or "")
        if "unpaired" in msg.lower():
            return {"status": "unpaired", "tasks": []}
        return {"status": "error", "tasks": []}
    if doc.get("isError") is True:
        return {"status": "error", "tasks": []}
    if "result" in doc and ("jsonrpc" in doc or isinstance(doc.get("result"), dict)):
        doc = doc.get("result")
        if not isinstance(doc, dict):
            return {"status": "error", "tasks": []}
    if doc.get("isError") is True:
        return {"status": "error", "tasks": []}
    if "content" in doc and "tasks" not in doc:
        parts = []
        for item in doc.get("content") or []:
            if isinstance(item, dict) and item.get("type") == "text":
                parts.append(item.get("text") or "")
        blob = "".join(parts).strip()
        if not blob:
            return {"status": "error", "tasks": []}
        try:
            doc = json.loads(blob)
        except ValueError:
            return {"status": "error", "tasks": []}
        if not isinstance(doc, dict):
            return {"status": "error", "tasks": []}
    if "tasks" in doc and "status" not in doc:
        doc = dict(doc)
        doc["status"] = "ok"
    return doc


def pick(doc):
    doc = unwrap(doc)
    status = doc.get("status")
    if status == "unpaired":
        return "unpaired"
    if status != "ok":
        return "error"
    tasks = doc.get("tasks")
    if not isinstance(tasks, list):
        return "error"

    tasks = [task for task in tasks if isinstance(task, dict) and _id(task)]
    # A failed/no-op session should not starve other eligible tasks this tick.
    excluded = set(os.environ.get("PISO_TASK_EXCLUDE", "").split(","))
    tasks = [task for task in tasks if _id(task) not in excluded]
    held = [t for t in tasks if t.get("resumable") is True]
    if held:
        return "resume " + _choose(held)
    candidates = [t for t in tasks if t.get("claimable") is True]
    if candidates:
        return "start " + _choose(candidates)
    return "sleep"


def main():
    raw = sys.stdin.read()
    if not raw.strip():
        print("error")
        return
    try:
        doc = json.loads(raw)
    except ValueError:
        print("error")
        return
    print(pick(doc))


if __name__ == "__main__":
    main()
