#!/bin/bash
# Exercises piso-mcp-add against a fake curl. No gateway required.
set -u
cd "$(dirname "$0")"

fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/bin" "$root/lib"

cat >"$root/bin/curl" <<'EOF'
#!/usr/bin/env python3
import os, sys
args = sys.argv[1:]
out = body = url = None
i = 0
while i < len(args):
    a = args[i]
    if a in ("-o", "-d", "-w", "-H", "-X", "--max-time"):
        val = args[i + 1]
        if a == "-o":
            out = val
        elif a == "-d":
            body = val
        i += 2
        continue
    if a.startswith("-"):
        i += 1
        continue
    url = a
    i += 1
open(os.environ["CURL_BODY_LOG"], "w").write(body or "")
open(os.environ["CURL_URL_LOG"], "w").write(url or "")
mode = os.environ.get("CURL_MODE", "ok")
if mode == "down":
    sys.exit(7)
if mode == "bad":
    open(out, "w").write('{"error":"url must use HTTPS"}')
    print("400")
    sys.exit(0)
if mode == "token":
    open(out, "w").write('{"name":"ai-boost","url":"https://mcp.example.com/mcp","placeholder":"sk-live-secret","status":"ok"}')
    print("201")
    sys.exit(0)
open(out, "w").write('{"name":"ai-boost","url":"https://mcp.example.com/mcp","placeholder":"piso_mcp_piso_ai-boost","status":"needs-auth"}')
print("201")
EOF
chmod +x "$root/bin/curl"
cp gateway-post.sh "$root/lib/piso-gateway-post.sh"

export PATH="$root/bin:$PATH"
export GATEWAY_URL="http://gateway:8083"
export PISO_WORKER_NAME="piso-worker-piso"
export PISO_WORKER_SLUG="piso"
export PISO_GATEWAY_POST_SH="$root/lib/piso-gateway-post.sh"
export CURL_BODY_LOG="$root/body.json"
export CURL_URL_LOG="$root/url.txt"

mcp="$root/mcp.json"
cat >"$mcp" <<'EOF'
{
  "mcpServers": {
    "old-boost": { "url": "https://mcp.example.com/mcp/" },
    "other": { "command": "npx", "args": ["-y", "some-server"] }
  },
  "autoEnableCodemode": false
}
EOF
export PISO_MCP_JSON="$mcp"
export CURL_MODE=ok

out=$(bash ./mcp-add.sh "https://mcp.example.com/mcp" "AI Boost")
status=$?
if [ "$status" -eq 0 ]; then ok "exit 0"; else bad "exit 0 (got $status)"; fi
printf '%s\n' "$out" | grep -q 'status: needs-auth' && ok "prints needs-auth" || bad "prints needs-auth"
printf '%s\n' "$out" | grep -q 'Do not run pi mcp login' && ok "forbids pi mcp login" || bad "forbids pi mcp login"

python3 - "$mcp" "$root/body.json" "$root/url.txt" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
body = json.load(open(sys.argv[2]))
url = open(sys.argv[3]).read()
assert url.endswith("/api/v1/worker/mcp"), url
assert body["worker"] == "piso-worker-piso"
assert body["slug"] == "piso"
assert body["url"] == "https://mcp.example.com/mcp"
assert body["name"] == "AI Boost"
servers = doc["mcpServers"]
assert "old-boost" not in servers, servers
assert servers["other"]["command"] == "npx"
entry = servers["ai-boost"]
assert entry["headers"]["Authorization"] == "Bearer piso_mcp_piso_ai-boost"
assert "oauth" not in entry and "bearerToken" not in entry and "auth" not in entry
assert doc["autoEnableCodemode"] is False
PY
if [ $? -eq 0 ]; then ok "mcp.json shape"; else bad "mcp.json shape"; fi

cp "$mcp" "$root/before.json"
export CURL_MODE=bad
bash ./mcp-add.sh "https://mcp.example.com/mcp" >/dev/null
status=$?
if [ "$status" -eq 1 ]; then ok "400 exits 1"; else bad "400 exits 1 (got $status)"; fi
cmp -s "$mcp" "$root/before.json" && ok "400 does not rewrite mcp.json" || bad "400 does not rewrite mcp.json"

export CURL_MODE=token
bash ./mcp-add.sh "https://mcp.example.com/mcp" >/dev/null
status=$?
if [ "$status" -eq 1 ]; then ok "real token exits 1"; else bad "real token exits 1 (got $status)"; fi
cmp -s "$mcp" "$root/before.json" && ok "real token does not rewrite mcp.json" || bad "real token does not rewrite mcp.json"
if ! grep -q 'sk-live-secret' "$mcp"; then ok "secret absent"; else bad "secret absent"; fi

env -u PISO_WORKER_SLUG \
  PATH="$PATH" GATEWAY_URL="$GATEWAY_URL" PISO_WORKER_NAME="$PISO_WORKER_NAME" \
  PISO_GATEWAY_POST_SH="$PISO_GATEWAY_POST_SH" PISO_MCP_JSON="$mcp" \
  CURL_BODY_LOG="$CURL_BODY_LOG" CURL_URL_LOG="$CURL_URL_LOG" CURL_MODE=ok \
  bash ./mcp-add.sh "https://mcp.example.com/mcp" >/dev/null
status=$?
if [ "$status" -eq 2 ]; then ok "missing slug exits 2"; else bad "missing slug exits 2 (got $status)"; fi

if [ "$fail" -ne 0 ]; then
  echo "mcp-add tests failed" >&2
  exit 1
fi
echo "mcp-add tests passed"
