#!/usr/bin/env python3
"""Mechanical inbox picker for piso-task-watch.

Reads one JSON document from stdin (raw peek, MCP tools/call result, or
JSON-RPC envelope). Prints a single line:

  resume <id>
  start <id>
  sleep
  unpaired
  error

Never reads title, body, plan, or repo. heldByMe / claimable / assignedToMe
are taken as booleans from the coordinator.
"""
from __future__ import print_function

import json
import sys


def _priority(task):
    try:
        return int(task.get("priority") or 0)
    except (TypeError, ValueError):
        return 0


def _id(task):
    value = task.get("id")
    if value is None:
        return ""
    text = str(value).strip()
    if not text or any(ch.isspace() for ch in text):
        return ""
    return text


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
    if "result" in doc and ("jsonrpc" in doc or "content" in (doc.get("result") or {})):
        doc = doc.get("result")
        if not isinstance(doc, dict):
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

    def sort_key(task, assigned_first):
        assigned = 0 if task.get("assignedToMe") is True else 1
        ident = _id(task)
        if assigned_first:
            return (assigned, -_priority(task), ident)
        return (-_priority(task), ident)

    held = []
    for task in tasks:
        if not isinstance(task, dict):
            continue
        if task.get("heldByMe") is True and _id(task):
            held.append(task)
    held.sort(key=lambda t: sort_key(t, False))
    if held:
        return "resume " + _id(held[0])

    candidates = []
    for task in tasks:
        if not isinstance(task, dict):
            continue
        if task.get("heldByMe") is True:
            continue
        if task.get("claimable") is not True:
            continue
        if not _id(task):
            continue
        candidates.append(task)
    candidates.sort(key=lambda t: sort_key(t, True))
    if candidates:
        return "start " + _id(candidates[0])
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
