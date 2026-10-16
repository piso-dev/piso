#!/usr/bin/env python3
"""Ensure mcp.json lists the encrypted gateway channel as a stdio server.

Remote entries (url / headers) are left untouched. A corrupt mcp.json is
not overwritten.
"""
import json
import os
import sys
import tempfile

dest = os.environ.get("PISO_MCP_JSON", "/root/.pi/agent/mcp.json")
command = os.environ.get("PISO_MCP_COMMAND", "/usr/local/bin/piso-mcp")
name = "piso"

doc = {}
if os.path.exists(dest):
    raw = open(dest).read().strip()
    if raw:
        try:
            doc = json.loads(raw)
        except Exception:
            sys.stderr.write("piso-mcp: existing mcp.json is not json; not overwritten\n")
            sys.exit(1)
if not isinstance(doc, dict):
    sys.stderr.write("piso-mcp: mcp.json is not an object; not overwritten\n")
    sys.exit(1)
servers = doc.get("mcpServers")
if servers is None:
    servers = {}
if not isinstance(servers, dict):
    sys.stderr.write("piso-mcp: mcpServers is not an object; not overwritten\n")
    sys.exit(1)

want = {
    "command": command,
    "args": [],
    "description": "encrypted channel to the piso gateway; do not put secrets here",
}
cur = servers.get(name)
if cur == want:
    sys.exit(0)
servers[name] = want
doc["mcpServers"] = servers

parent = os.path.dirname(dest) or "."
os.makedirs(parent, exist_ok=True)
fd, tmp = tempfile.mkstemp(dir=parent, prefix=".mcp.json.")
try:
    with os.fdopen(fd, "w") as f:
        json.dump(doc, f, indent=2)
        f.write("\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, dest)
except Exception:
    try:
        os.unlink(tmp)
    except OSError:
        pass
    raise
