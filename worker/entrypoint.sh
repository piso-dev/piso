# piso worker entrypoint
# Installs the gateway's MITM CA into the system trust store (so curl/git/node
# all accept the gateway's certs), then execs the requested command
# (default: keep alive for `piso attach`).
set -e

if [ -f /piso-ca.pem ]; then
  cp /piso-ca.pem /usr/local/share/ca-certificates/piso-gateway.crt 2>/dev/null || true
  update-ca-certificates >/dev/null 2>&1 || true
fi

exec "$@"