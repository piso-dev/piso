#!/usr/bin/env bash
# smoke.sh — end-to-end verification of the piso gateway (no Docker needed):
#   substitute, block (real secret + pattern), block→rule→retry, ingress, log.
# Expects `make build` first; gateway runs locally on 127.0.0.1:18080-2.
set -euo pipefail
cd "$(dirname "$0")/.."

GW=${GW:-bin/gateway}
WORK=$(mktemp -d)
# kill any stale local gateways from previous runs (pattern matches the binary
# invocation, not just a name)
TARGETS=$(pgrep -f "gateway -state" || true)
[ -n "$TARGETS" ] && kill $TARGETS 2>/dev/null || true
trap 'kill $GW_PID 2>/dev/null || true; rm -rf "$WORK"' EXIT

echo "== starting gateway =="
"$GW" \
  -state "$WORK/state.json" -patterns "$WORK/patterns.json" -log "$WORK/req.jsonl" \
  -ca-cert "$WORK/ca.crt" -ca-key "$WORK/ca.key" \
  -proxy-listen 127.0.0.1:18080 -ctrl-listen 127.0.0.1:18081 -worker-listen 127.0.0.1:18083 -ingress-listen 127.0.0.1:18082 \
  > "$WORK/gw.log" 2>&1 &
GW_PID=$!
sleep 1

API=http://127.0.0.1:18081/api/v1
WAPI=http://127.0.0.1:18083/api/v1
P="--proxy http://127.0.0.1:18080 --cacert $WORK/ca.crt"
pass=0; fail=0
check() { # check <desc> <cond>
  if eval "$2"; then echo "  ✓ $1"; pass=$((pass+1)); else echo "  ✗ $1"; fail=$((fail+1)); fi
}

# wait for the gateway + warm its DNS path
for i in $(seq 1 20); do curl -s -o /dev/null http://127.0.0.1:18081/api/v1/health && break; sleep 0.5; done

echo "== 1. substitution =="
curl -s -X POST "$API/secrets" -H 'Content-Type: application/json' \
  -d '{"name":"httpbin","placeholder":"piso_httpbin_abc123","value":"sk-REAL-KEY-123456789","allowedHosts":["httpbin.org"]}' >/dev/null
curl -s -X POST "$API/rules" -H 'Content-Type: application/json' \
  -d '{"placeholder":"piso_httpbin_abc123","host":"httpbin.org","secretId":"x"}' >/dev/null
AUTH=$(curl -s $P -H "Authorization: Bearer piso_httpbin_abc123" https://httpbin.org/headers | python3 -c "import json,sys; print(json.load(sys.stdin)['headers']['Authorization'])")
check "placeholder substituted on egress" "[ \"$AUTH\" = 'Bearer sk-REAL-KEY-123456789' ]"

echo "== 2. real-secret block =="
curl -s $P -D /tmp/h -H "Authorization: Bearer sk-REAL-KEY-123456789 extra" -o /dev/null https://httpbin.org/anything || true
check "real secret blocked with 407" "grep -q '407' /tmp/h"
check "block carries request id header" "grep -qi 'X-Piso-Request-Id' /tmp/h"

echo "== 3. credential-pattern block =="
curl -s $P -H "Authorization: Bearer ghp_012345678901234567890123456789012345" -o /dev/null https://httpbin.org/anything || true
R=$(curl -s "$API/requests" | python3 -c "import json,sys; rs=[r for r in json.load(sys.stdin) if r['action']=='block' and 'credential-pattern' in r['reasons']]; print(rs[0]['id'] if rs else '')")
check "github PAT pattern blocked + logged" "[ -n \"$R\" ]"

echo "== 4. no-rule → add rule → retry =="
curl -s $P -D /tmp/h2 -H "Authorization: Bearer piso_norule_zzz" -o /dev/null https://httpbin.org/anything || true
RID=$(curl -s "$API/requests" | python3 -c "import json,sys; rs=[r for r in json.load(sys.stdin) if r['action']=='block' and 'no-secret-rule' in r['reasons']]; print(rs[0]['id'] if rs else '')")
check "no-rule placeholder blocked retryable" "[ -n \"$RID\" ]"
curl -s -X POST "$API/secrets" -H 'Content-Type: application/json' \
  -d '{"name":"norule","placeholder":"piso_norule_zzz","value":"sk-NORULE-REAL-1"}' >/dev/null
curl -s -X POST "$API/rules" -H 'Content-Type: application/json' \
  -d '{"placeholder":"piso_norule_zzz","host":"httpbin.org","secretId":"x"}' >/dev/null
ST=""
if [ -n "$RID" ]; then
  ST=$(curl -s -X POST "$API/requests/$RID/retry" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))")
fi
check "retry after rule added → upstream 200" "[ \"$ST\" = 200 ]"

echo "== 5. ingress =="
python3 -m http.server 19999 --bind 127.0.0.1 >/dev/null 2>&1 & SRV=$!
sleep 0.5
curl -s -X POST "$API/routes" -H 'Content-Type: application/json' \
  -d '{"name":"preview","worker":"127.0.0.1","port":19999}' >/dev/null
BODY=$(curl -s -H "Host: preview.piso.local" http://127.0.0.1:18082/ | head -1)
check "ingress routes preview.piso.local → worker" "echo '$BODY' | grep -qi '<!DOCTYPE'"
kill $SRV 2>/dev/null || true

echo "== 6. log safety =="
LEAK=$(grep -c "sk-REAL-KEY-123456789" "$WORK/req.jsonl" || true)
check "real value never in request log" "[ \"$LEAK\" = 0 ]"

echo "== 7. planning ingress approve =="
python3 -m http.server 19998 --bind 127.0.0.1 >/dev/null 2>&1 & PLAN=$!
sleep 0.5
CTRL_CREATE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/ingress/requests" -H 'Content-Type: application/json' \
  -d '{"kind":"planning","worker":"127.0.0.1","port":19998,"slug":"smoke"}' || true)
check "control plane refuses worker planning create" "[ \"$CTRL_CREATE\" != 200 ] && [ \"$CTRL_CREATE\" != 201 ]"
HOST_ON_WORKER=$(curl -s -o /dev/null -w '%{http_code}' "$WAPI/secrets" || true)
check "worker API does not expose host secrets" "[ \"$HOST_ON_WORKER\" = 404 ]"
PEND=$(curl -s -X POST "$WAPI/worker/planning" -H 'Content-Type: application/json' \
  -d '{"kind":"planning","worker":"127.0.0.1","port":19998,"slug":"smoke"}')
ING_ID=$(python3 -c "import json,sys; print(json.load(sys.stdin)['id'])" <<<"$PEND")
check "planning request is pending" "[ -n \"$ING_ID\" ]"
MISS=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: plan-smoke.piso.local" http://127.0.0.1:18082/ || true)
check "pending plan is not routed yet" "[ \"$MISS\" = 404 ]"
curl -s -X POST "$API/ingress/requests/$ING_ID/approve" >/dev/null
PLAN_BODY=$(curl -s -H "Host: plan-smoke.piso.local" http://127.0.0.1:18082/ | head -1)
check "approve opens plan-smoke.piso.local" "echo '$PLAN_BODY' | grep -qi '<!DOCTYPE'"
kill $PLAN 2>/dev/null || true

echo
echo "passed: $pass  failed: $fail"
[ "$fail" = 0 ] || exit 1
echo "SMOKE OK"