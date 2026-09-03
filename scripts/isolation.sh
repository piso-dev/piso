#!/usr/bin/env bash
# isolation.sh — Docker-level proof that a worker on piso_vpc cannot reach the
# internet except through the gateway (which NATs out piso_egress).
#
#   1. Wipe piso containers/networks (testing: no migration).
#   2. compose up the gateway so piso_vpc is created internal.
#   3. noproxy from vpc → 1.1.1.1 must fail.
#   4. HTTP via the gateway proxy from vpc must succeed.
#   5. Direct HTTP from piso_egress must succeed (gateway still has a NAT path).
set -euo pipefail
cd "$(dirname "$0")/.."
export PISO_DATA="${PISO_DATA:-$HOME/.piso}"
export PISO_PROXY_PORT="${PISO_PROXY_PORT:-8080}"
export PISO_CTRL_PORT="${PISO_CTRL_PORT:-8081}"
export PISO_INGRESS_PORT="${PISO_INGRESS_PORT:-8082}"
mkdir -p "$PISO_DATA"

PROBE_IMAGE=${PROBE_IMAGE:-piso-isolation-probe}
VPC=piso_vpc
EGRESS=piso_egress
GATEWAY=piso-gateway
DIRECT_URL=http://1.1.1.1
PROXY_URL=http://example.com

pass=0
fail=0
check() { # check <desc> <cond>
  if eval "$2"; then echo "  ✓ $1"; pass=$((pass+1)); else echo "  ✗ $1"; fail=$((fail+1)); fi
}

echo "== resetting piso runtime =="
IDS=$(docker ps -aq --filter name=piso- || true)
if [ -n "$IDS" ]; then
  # shellcheck disable=SC2086
  docker rm -f $IDS >/dev/null
fi
docker network rm "$VPC" "$EGRESS" >/dev/null 2>&1 || true

echo "== starting gateway (creates internal $VPC) =="
docker compose -f compose/gateway.yaml -p piso up -d --build -t 0
for i in $(seq 1 30); do
  curl -s -o /dev/null http://127.0.0.1:8081/api/v1/health && break
  sleep 1
done
curl -s -o /dev/null http://127.0.0.1:8081/api/v1/health \
  || { echo "gateway control plane did not become healthy"; exit 1; }

INTERNAL=$(docker network inspect "$VPC" --format '{{.Internal}}')
check "$VPC is Internal=true" "[ \"$INTERNAL\" = true ]"

echo "== building probe image $PROBE_IMAGE (alpine + curl; build has host network) =="
docker build -t "$PROBE_IMAGE" - >/dev/null <<'EOF'
FROM alpine:3.20
RUN apk add --no-cache curl
EOF

# curl --noproxy '*' bypasses HTTP_PROXY. Timeout 5s so a DROP does not hang.
echo "== 1. direct egress from $VPC must fail =="
if docker run --rm --network "$VPC" --name piso-isolation-vpc-direct "$PROBE_IMAGE" \
  curl -sS -o /dev/null --max-time 5 --noproxy '*' "$DIRECT_URL"; then
  DIRECT_OK=1
else
  DIRECT_OK=0
fi
check "noproxy curl $DIRECT_URL from $VPC fails" "[ \"$DIRECT_OK\" = 0 ]"

echo "== 2. proxied egress from $VPC must work =="
PROXY_CODE=$(docker run --rm --network "$VPC" --name piso-isolation-vpc-proxy "$PROBE_IMAGE" \
  curl -sS -o /dev/null --max-time 10 -x "http://${GATEWAY}:8080" -w '%{http_code}' "$PROXY_URL" || true)
check "curl -x ${GATEWAY}:8080 $PROXY_URL from $VPC returns 200" "[ \"$PROXY_CODE\" = 200 ]"

echo "== 3. direct egress from $EGRESS must work =="
if docker run --rm --network "$EGRESS" --name piso-isolation-egress "$PROBE_IMAGE" \
  curl -sS -o /dev/null --max-time 10 --noproxy '*' "$DIRECT_URL"; then
  EGRESS_OK=1
else
  EGRESS_OK=0
fi
check "noproxy curl $DIRECT_URL from $EGRESS succeeds" "[ \"$EGRESS_OK\" = 1 ]"

echo
echo "passed: $pass  failed: $fail"
[ "$fail" = 0 ] || exit 1
echo "ISOLATION OK"
