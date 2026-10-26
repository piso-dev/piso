#!/bin/bash
# Exercises piso-workbench with a sandboxed PISO_WORKBENCH and a file:// Go
# download base. No network, no real toolchain download.
set -u
cd "$(dirname "$0")"
W=$PWD/workbench.sh

fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
export PISO_WORKBENCH="$root/wb"
export PISO_WORKBENCH_GO_BASE="file://$root/dl"

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  *) arch=arm64 ;;
esac

# A fake Go toolchain: go/bin/{go,gofmt}. $1=version string, $2=exit code of `go`.
fake_go_tarball() {
  local v=$1 rc=${2:-0} stage
  stage=$(mktemp -d "$root/stage.XXXX")
  mkdir -p "$stage/go/bin"
  cat >"$stage/go/bin/go" <<EOF
#!/bin/sh
if [ \$# -gt 0 ] && [ "\$1" = version ]; then
  echo "go version go$v linux/$arch"
  exit $rc
fi
exit $rc
EOF
  printf '#!/bin/sh\nexit 0\n' >"$stage/go/bin/gofmt"
  chmod +x "$stage/go/bin/go" "$stage/go/bin/gofmt"
  mkdir -p "$root/dl"
  tar czf "$root/dl/go$v.linux-$arch.tar.gz" -C "$stage" go
  rm -rf "$stage"
}

# --- env ---------------------------------------------------------------------
out=$(eval "$(PISO_WORKBENCH=$PISO_WORKBENCH "$W" env)" 2>/dev/null; printf '%s|%s|%s|%s' "$PISO_WORKBENCH" "$GOPATH" "$GOCACHE" "$PATH")
case "$out" in
  "$root/wb|$root/wb/gopath|$root/wb/gocache|$root/wb/bin:"*) ok "env exports workbench-rooted vars and prefixes PATH" ;;
  *) bad "env exports unexpected: $out" ;;
esac

# Nothing the worker installs may land in the project checkout.
if "$W" env | grep -q '^export .*=.*"/workspace'; then
  bad "env points at /workspace"
else
  ok "env never points into /workspace"
fi

# --- path / scratch ----------------------------------------------------------
[ -d "$("$W" path bin)" ] && ok "path bin creates the dir" || bad "path bin did not create the dir"
"$W" path nope >/dev/null 2>&1 && bad "path accepted an unknown subdir" || ok "path rejects unknown subdir"

scr=$("$W" scratch build)
case "$scr" in
  "$root/wb/tmp/build."*) ok "scratch is a fresh dir under workbench/tmp" ;;
  *) bad "scratch returned $scr" ;;
esac
printf '#!/bin/sh\necho scratch-ran\n' >"$scr/x"
chmod +x "$scr/x"
if got=$("$scr/x" 2>/dev/null); then
  [ "$got" = scratch-ran ] && ok "scratch binaries are executable" || bad "scratch binary printed $got"
else
  bad "scratch binaries are not executable (noexec filesystem?)"
fi

# --- install go --------------------------------------------------------------
fake_go_tarball 9.9.9
"$W" install go@9.9.9 >/dev/null 2>&1
[ -x "$PISO_WORKBENCH/bin/go" ] && ok "install links go into bin/" || bad "install did not link bin/go"
[ -x "$PISO_WORKBENCH/bin/gofmt" ] && ok "install links gofmt into bin/" || bad "install did not link bin/gofmt"
ver=$(GOROOT=$PISO_WORKBENCH/versions/go9.9.9 "$PISO_WORKBENCH/bin/go" version)
case "$ver" in
  "go version go9.9.9"*) ok "installed toolchain reports its version" ;;
  *) bad "installed toolchain reported: $ver" ;;
esac
[ -d "$PISO_WORKBENCH/versions/go9.9.9/bin" ] && ok "GOROOT has bin/ at its top level (leading go/ stripped)" || bad "GOROOT layout is wrong"
[ -f "$PISO_WORKBENCH/downloads/go9.9.9.linux-$arch.tar.gz" ] && ok "tarball cached in downloads/" || bad "tarball not cached"

# Already installed: no second download, still succeeds.
before=$(ls -l --time-style=+%s "$PISO_WORKBENCH/downloads"/*.tar.gz | awk '{print $6}')
PISO_WORKBENCH_GO_BASE="file://$root/does-not-exist" "$W" install go@9.9.9 >/dev/null 2>&1 \
  && ok "reinstall is a no-op (offline base still succeeds)" || bad "reinstall tried to download again"
after=$(ls -l --time-style=+%s "$PISO_WORKBENCH/downloads"/*.tar.gz | awk '{print $6}')
[ "$before" = "$after" ] && ok "reinstall left the cached tarball untouched" || bad "reinstall rewrote the tarball"

# A toolchain that cannot run here (wrong arch / corrupt) must be rejected
# before it is published, or `go` fails later with confusing linker errors.
fake_go_tarball 9.9.8 1
PISO_WORKBENCH_GO_BASE="file://$root/dl" "$W" install go@9.9.8 >/dev/null 2>&1 \
  && bad "install accepted a toolchain that does not run" || ok "install rejects a toolchain that does not run"
[ -e "$PISO_WORKBENCH/versions/go9.9.8" ] && bad "failed install left a broken GOROOT" || ok "failed install leaves no GOROOT"

"$W" install go@bad >/dev/null 2>&1 && bad "install accepted a malformed version" || ok "install rejects a malformed version"
"$W" install go >/dev/null 2>&1 && bad "install accepted a missing version" || ok "install requires a version"

# Non-Go tools are not downloaded from GitHub releases (the gateway 504s/407s
# them); the failure must point at the host-request path.
err=$("$W" install rg@14 2>&1)
rc=$?
[ "$rc" = 2 ] && ok "install of an unsupported tool exits 2" || bad "unsupported tool exited $rc"
case "$err" in
  *HOST\ ACTION\ REQUEST*) ok "unsupported tool tells the worker to emit a host_request" ;;
  *) bad "unsupported tool message lacks host-request guidance: $err" ;;
esac

# --- run ---------------------------------------------------------------------
got=$("$W" run sh -c 'printf %s "$GOBIN"')
[ "$got" = "$PISO_WORKBENCH/bin" ] && ok "run applies the workbench env" || bad "run env GOBIN=$got"

# --- clean -------------------------------------------------------------------
mkdir -p "$PISO_WORKBENCH/gocache/x" "$PISO_WORKBENCH/gopath/pkg/mod/x" "$PISO_WORKBENCH/tmp/old"
printf 'junk' >"$PISO_WORKBENCH/gocache/x/f"
"$W" clean --caches >/dev/null 2>&1
[ -z "$(ls -A "$PISO_WORKBENCH/gocache")" ] && ok "clean --caches empties GOCACHE" || bad "clean --caches left GOCACHE populated"
[ -z "$(ls -A "$PISO_WORKBENCH/downloads")" ] && ok "clean --caches empties downloads/" || bad "clean --caches left downloads populated"
[ -d "$PISO_WORKBENCH/gopath/pkg/mod/x" ] && ok "clean --caches keeps the module cache" || bad "clean --caches dropped the module cache"
[ -x "$PISO_WORKBENCH/bin/go" ] && ok "clean --caches keeps installed binaries" || bad "clean --caches removed bin/go"

"$W" clean --scratch >/dev/null 2>&1
[ -z "$(ls -A "$PISO_WORKBENCH/tmp")" ] && ok "clean --scratch empties tmp/" || bad "clean --scratch left tmp populated"
[ -d "$PISO_WORKBENCH/versions/go9.9.9" ] && ok "clean --scratch keeps toolchains" || bad "clean --scratch removed the toolchain"

# --- status ------------------------------------------------------------------
st=$("$W" status)
case "$st" in
  *"workbench: $PISO_WORKBENCH"*"total"*) ok "status reports the root and total size" ;;
  *) bad "status output unexpected: $st" ;;
esac

# --- refusing bad roots ------------------------------------------------------
PISO_WORKBENCH=/workspace/.workbench "$W" status >/dev/null 2>&1 \
  && bad "accepted a workbench inside the git repo" || ok "refuses /workspace"
PISO_WORKBENCH=/tmp/wb "$W" status >/dev/null 2>&1 \
  && bad "accepted a noexec tmpfs workbench" || ok "refuses /tmp"

# --- misc --------------------------------------------------------------------
"$W" help | grep -q 'workbench install go@' && ok "help lists install" || bad "help lacks install"
"$W" frobnicate >/dev/null 2>&1 && bad "unknown command succeeded" || ok "unknown command exits nonzero"
bash -n "$W" && ok "script parses" || bad "script has a syntax error"

[ "$fail" = 0 ] && echo "workbench: all tests passed" || echo "workbench: FAILURES" >&2
exit "$fail"
