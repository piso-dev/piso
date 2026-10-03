#!/bin/bash
# piso-mcp-add — register a remote MCP server with the gateway and write
# ~/.pi/agent/mcp.json so pi sends a piso_mcp_… placeholder instead of
# starting in-worker OAuth.
#
#   piso-mcp-add https://mcp.example.com/mcp [name]
#
# The host finishes login on the dashboard (MCP tab / Authorize chip). This
# command never opens a browser and never stores a real token.
#
# Do not use `pi mcp add` or `pi mcp login` in a worker. Those start OAuth
# inside the container; the callback is http://127.0.0.1:<port>/callback,
# which the host browser cannot reach.
set -u

usage() {
  echo "usage: piso-mcp-add <https-url> [name]" >&2
  exit 2
}

url="${1:-}"
name="${2:-}"
[ -n "$url" ] || usage
[ -z "${3:-}" ] || usage

case "$url" in
  https://*) ;;
  *) echo "piso-mcp-add: url must be https" >&2; exit 2 ;;
esac
case "$url" in
  *[@#]*) echo "piso-mcp-add: url must not contain credentials or a fragment" >&2; exit 2 ;;
esac

gw="${GATEWAY_URL:-}"
worker="${PISO_WORKER_NAME:-}"
slug="${PISO_WORKER_SLUG:-}"
mcp_json="${PISO_MCP_JSON:-/root/.pi/agent/mcp.json}"

if [ -z "$gw" ] || [ -z "$worker" ] || [ -z "$slug" ]; then
  echo "piso-mcp-add: GATEWAY_URL/PISO_WORKER_NAME/PISO_WORKER_SLUG not set" >&2
  exit 2
fi

post_sh="${PISO_GATEWAY_POST_SH:-/usr/local/lib/piso-gateway-post.sh}"
if [ ! -f "$post_sh" ]; then
  echo "piso-mcp-add: missing $post_sh" >&2
  exit 2
fi
# shellcheck disable=SC1090
. "$post_sh"

json_esc() {
  local v="$1"
  v=$(printf '%s' "$v" | tr '\r\n\t' '   ')
  v="${v//\\/\\\\}"
  v="${v//\"/\\\"}"
  printf '%s' "$v"
}

body="{\"worker\":\"$(json_esc "$worker")\",\"slug\":\"$(json_esc "$slug")\",\"url\":\"$(json_esc "$url")\""
if [ -n "$name" ]; then
  body="$body,\"name\":\"$(json_esc "$name")\""
fi
body="$body}"

out=$(mktemp)
trap 'rm -f "$out"' EXIT
code=$(piso_gateway_post "${gw%/}/api/v1/worker/mcp" "$body" "$out")
if [ "$code" != "200" ] && [ "$code" != "201" ]; then
  err=$(python3 -c 'import json,sys
try:
    print(json.load(open(sys.argv[1])).get("error") or "gateway error")
except Exception:
    print("gateway error")' "$out" 2>/dev/null || echo "gateway error")
  echo "piso-mcp-add: $err (HTTP $code)" >&2
  exit 1
fi

python3 - "$out" "$mcp_json" <<'PY'
import json, os, re, sys, tempfile

resp_path, dest = sys.argv[1], sys.argv[2]
try:
    resp = json.load(open(resp_path))
except Exception:
    sys.stderr.write("piso-mcp-add: gateway response was not json; mcp.json not written\n")
    sys.exit(1)

name = resp.get("name") if isinstance(resp.get("name"), str) else ""
url = resp.get("url") if isinstance(resp.get("url"), str) else ""
placeholder = resp.get("placeholder") if isinstance(resp.get("placeholder"), str) else ""
status = resp.get("status") if isinstance(resp.get("status"), str) else ""

if not re.fullmatch(r"[A-Za-z0-9_-]+", name):
    sys.stderr.write("piso-mcp-add: refused gateway name; mcp.json not written\n")
    sys.exit(1)
if not url.startswith("https://") or not re.fullmatch(r"piso_mcp_[A-Za-z0-9_-]+", placeholder):
    sys.stderr.write("piso-mcp-add: refused non-placeholder credential; mcp.json not written\n")
    sys.exit(1)

doc = {}
if os.path.exists(dest):
    raw = open(dest).read().strip()
    if raw:
        try:
            doc = json.loads(raw)
        except Exception:
            sys.stderr.write("piso-mcp-add: existing mcp.json is not json; not overwritten\n")
            sys.exit(1)
if not isinstance(doc, dict):
    sys.stderr.write("piso-mcp-add: mcp.json is not an object; not overwritten\n")
    sys.exit(1)
servers = doc.get("mcpServers")
if servers is None:
    servers = {}
if not isinstance(servers, dict):
    sys.stderr.write("piso-mcp-add: mcpServers is not an object; not overwritten\n")
    sys.exit(1)

def same_url(a, b):
    return a.rstrip("/").lower() == b.rstrip("/").lower()

for key in [k for k, entry in servers.items()
            if k == name or (isinstance(entry, dict) and isinstance(entry.get("url"), str) and same_url(entry["url"], url))]:
    del servers[key]

# Authorization header is what stops current pi from starting in-worker OAuth.
# auth/bearerToken is ignored and must not be written.
servers[name] = {
    "url": url,
    "headers": {"Authorization": "Bearer " + placeholder},
    "description": "piso-managed; sign in on the dashboard, not pi mcp login",
}
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

print("piso-mcp-add: registered")
print("name: " + name)
print("url: " + url)
print("placeholder: " + placeholder)
print("status: " + status)
print("mcp.json: " + dest)
if status in ("needs-auth", "expired", "revoked"):
    print("next: Ask the human to Authorize " + name + " on the dashboard MCP tab (or the Authorize chip). Do not run pi mcp login. Do not paste tokens. A connect before they sign in is held and replays after.")
elif status == "ok":
    print("next: Already signed in. A new session or /reload will connect. Do not run pi mcp login.")
else:
    print("next: status is " + (status or "unknown") + ". Do not run pi mcp login.")
PY
