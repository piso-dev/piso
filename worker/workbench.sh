#!/bin/bash
# piso-workbench — the worker's writable, EXECUTABLE toolchain home that is
# neither the git repo nor /tmp.
#
# Why this exists: /workspace is the host's checkout (anything downloaded there
# must be gitignored), /tmp and /dev/shm are tmpfs mounted noexec (a binary there
# returns 126 "Permission denied"), /var/tmp and bare /root are read-only. The
# agent volume ($HOME/.pi/agent) is real disk, exec-allowed and persistent, so
# toolchains and dependency caches live under it — no repo pollution.
#
#   WB=${PISO_WORKBENCH:-$HOME/.pi/agent/workbench}
#     bin/         installed executables (also on PATH at boot)
#     gopath/      GOPATH (module cache, go install targets)
#     gocache/     GOCACHE
#     versions/    unpacked Go toolchains
#     tmp/         scratch builds (exec allowed; /tmp is not)
#     downloads/   tarballs
#
# Usage:
#   eval "$(workbench env)"        export PATH/GOPATH/GOCACHE/... for this shell
#   workbench path [sub]           print (and create) a workbench dir
#   workbench scratch [name]       make a throwaway build dir under tmp/
#   workbench install go@1.25.0    unpack a Go toolchain into versions/ + bin/
#   workbench go <args...>         run go with the workbench env applied
#   workbench run <cmd...>         run any command with the workbench env applied
#   workbench status               sizes per dir + free space
#   workbench clean [--caches|--scratch|--all] [--older-than DAYS]
#   workbench env                  print export lines (use with eval)
set -u

WB=${PISO_WORKBENCH:-$HOME/.pi/agent/workbench}
GO_BASE=${PISO_WORKBENCH_GO_BASE:-https://dl.google.com/go}
GOPATH_DIR="$WB/gopath"
GOCACHE_DIR="$WB/gocache"

die() { echo "workbench: $*" >&2; exit 1; }

# Refuse a base dir inside the project checkout or on a no-exec tmpfs: the whole
# point is to stay out of the repo and to hold runnable binaries.
wb_init() {
  case "$WB" in
    /workspace|/workspace/*) die "refusing to use $WB (inside the git repo); set PISO_WORKBENCH" ;;
    /tmp|/tmp/*|/dev/shm|/dev/shm/*) die "refusing to use $WB (tmpfs is noexec); set PISO_WORKBENCH" ;;
  esac
  mkdir -p "$WB"/bin "$WB"/tmp "$WB"/downloads "$WB"/versions "$GOPATH_DIR" "$GOCACHE_DIR" \
    || die "cannot create $WB (is the agent volume writable?)"
}

arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "unsupported arch: $(uname -m)" ;;
  esac
}

print_env() {
  cat <<EOF
export PISO_WORKBENCH="$WB"
export PATH="$WB/bin:\$PATH"
export GOPATH="$GOPATH_DIR"
export GOMODCACHE="$GOPATH_DIR/pkg/mod"
export GOBIN="$WB/bin"
export GOCACHE="$GOCACHE_DIR"
export CGO_ENABLED="\${CGO_ENABLED:-0}"
export CARGO_HOME="$WB/cargo"
export RUSTUP_HOME="$WB/rustup"
export npm_config_prefix="$WB/npm"
export NPM_CONFIG_PREFIX="$WB/npm"
export PIP_CACHE_DIR="$WB/pip"
export UV_CACHE_DIR="$WB/uv"
export XDG_CACHE_HOME="$WB/cache"
EOF
}

# "1.25" -> "1.25.0"; a full X.Y.Z is used as given. Sets GO_VER instead of
# echoing: a `die` inside $(...) would only kill the subshell.
go_version() {
  case "$1" in
    *.*.*) GO_VER=$1 ;;
    *.*) GO_VER="$1.0" ;;
    *) die "go version must look like 1.25 or 1.25.0 (got '$1')" ;;
  esac
}

cmd_install() {
  local spec=${1:-} tool ver a tar url dest
  [ -n "$spec" ] || die "usage: workbench install go@1.25.0"
  tool=${spec%@*}
  ver=${spec#*@}
  [ "$tool" = "$spec" ] && ver=""
  case "$tool" in
    go)
      [ -n "$ver" ] || die "usage: workbench install go@1.25.0"
      go_version "$ver"
      ver=$GO_VER
      a=$(arch)
      dest="$WB/versions/go$ver"
      if [ -x "$dest/bin/go" ]; then
        ln -sfn "$dest/bin/go" "$WB/bin/go"
        ln -sfn "$dest/bin/gofmt" "$WB/bin/gofmt"
        echo "workbench: go $ver already installed at $dest"
        return 0
      fi
      tar="$WB/downloads/go$ver.linux-$a.tar.gz"
      url="$GO_BASE/go$ver.linux-$a.tar.gz"
      if [ -s "$tar" ] && tar tzf "$tar" >/dev/null 2>&1; then
        echo "workbench: using cached $tar"
      else
        echo "workbench: downloading $url"
        # GitHub *release* URLs are 504/407'd by the gateway; dl.google.com,
        # proxy.golang.org, pypi and npm are proxied fine.
        curl -fsSL --retry 2 --max-time 300 -o "$tar.part" "$url" \
          || { rm -f "$tar.part"; die "download failed: $url (if this host is blocked, emit a host_request instead of retrying)"; }
        mv "$tar.part" "$tar"
      fi
      local stage="$WB/versions/.stage.$$"
      rm -rf "$stage" && mkdir -p "$stage" || die "cannot create $stage"
      # Official tarballs have a leading go/ component; strip it so GOROOT is
      # the directory that holds bin/go directly.
      tar xzf "$tar" -C "$stage" --no-same-owner --strip-components=1 \
        || { rm -rf "$stage"; die "extract failed: $tar"; }
      [ -x "$stage/bin/go" ] || { rm -rf "$stage"; die "$tar does not look like a Go toolchain"; }
      # Prove the toolchain runs on THIS arch before publishing it. A toolchain
      # built for the wrong arch fails later with confusing linker errors
      # ("undefined: syscall.Open"), so fail here with the real reason.
      if ! GOROOT="$stage" "$stage/bin/go" version >/dev/null 2>&1; then
        GOROOT="$stage" "$stage/bin/go" version 2>&1 | tail -3 >&2
        rm -rf "$stage"
        die "unpacked go toolchain does not run here (arch $(arch)?)"
      fi
      rm -rf "$dest"
      mv "$stage" "$dest"
      ln -sfn "$dest/bin/go" "$WB/bin/go"
      ln -sfn "$dest/bin/gofmt" "$WB/bin/gofmt"
      GOROOT="$dest" "$dest/bin/go" version
      ;;
    *)
      cat >&2 <<EOF
workbench: don't know how to install '$tool'.
  - Go tools: workbench go install example.com/tool/cmd/tool@latest
  - Anything else with prebuilt release assets from GitHub: the gateway
    504s/407s releases/download and api.github.com, so ask the host:
      HOST ACTION REQUEST: fetch <url>
        COMMAND: curl -fsSL -o /workspace/.tools/<name> <url>   # or wherever the host prefers
        RESULT:  $WB/bin/<name>
EOF
      exit 2
      ;;
  esac
}

cmd_clean() {
  local what= days=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --caches) what=caches ;;
      --scratch) what=scratch ;;
      --all) what=all ;;
      --older-than) days=${2:-0}; shift ;;
      *) die "unknown clean flag: $1" ;;
    esac
    shift
  done
  [ -n "$what" ] || what=caches
  case "$what" in
    caches)
      rm -rf "$GOCACHE_DIR"/* "$WB"/downloads/* "$WB"/pip/* "$WB"/uv/* "$WB"/cache/* \
        "$WB"/cargo/registry/cache 2>/dev/null
      # GOMODCACHE is intentionally kept: it is the offline re-install cache.
      echo "workbench: pruned caches (kept gopath module cache)"
      ;;
    scratch)
      if [ -n "$days" ]; then
        find "$WB/tmp" -mindepth 1 -maxdepth 1 -mtime "+$days" -exec rm -rf {} + 2>/dev/null
        echo "workbench: pruned scratch older than ${days}d"
      else
        rm -rf "$WB"/tmp/* 2>/dev/null
        echo "workbench: pruned all scratch dirs"
      fi
      ;;
    all)
      rm -rf "$GOCACHE_DIR"/* "$WB"/downloads/* "$WB"/tmp/* "$GOPATH_DIR"/pkg/mod/cache 2>/dev/null
      echo "workbench: pruned caches + downloads + scratch (bin/ and versions/ kept)"
      ;;
  esac
  refresh_env_bin
}

# Installed Go binaries live in bin/; a wiped GOROOT symlink would leave a
# dangling `go`. Drop dangling links so `command -v go` is truthful.
refresh_env_bin() {
  local l
  for l in "$WB"/bin/*; do
    [ -L "$l" ] && [ ! -e "$l" ] && rm -f "$l"
  done
  return 0
}

cmd_status() {
  local d
  echo "workbench: $WB"
  for d in bin versions gopath gocache tmp downloads pip uv cache; do
    [ -e "$WB/$d" ] || continue
    printf '  %-10s %s\n' "$d" "$(du -sh "$WB/$d" 2>/dev/null | cut -f1)"
  done
  printf '  %-10s %s\n' "total" "$(du -sh "$WB" 2>/dev/null | cut -f1)"
  df -h "$WB" 2>/dev/null | awk 'NR==2 {printf "  fs         %s used of %s (%s free, %s)\n", $3, $2, $4, $5}'
  [ -e /workspace/.tools ] && echo "  note       /workspace/.tools still exists (gitignored legacy toolchain)"
  return 0
}

main() {
  local cmd=${1:-}
  shift || true
  case "$cmd" in
    env) wb_init; print_env ;;
    path)
      wb_init
      case "${1:-}" in
        "" ) echo "$WB" ;;
        bin|gopath|gocache|versions|tmp|downloads|cargo|rustup|npm|pip|uv|cache)
          mkdir -p "$WB/$1" && echo "$WB/$1" ;;
        *) die "unknown workbench dir: $1" ;;
      esac
      ;;
    scratch)
      wb_init
      mkdir -p "$WB/tmp"
      mktemp -d "$WB/tmp/${1:-scratch}.XXXXXX"
      ;;
    install) wb_init; cmd_install "$@" ;;
    go)
      wb_init
      eval "$(print_env)"
      exec go "$@"
      ;;
    run)
      wb_init
      [ $# -gt 0 ] || die "usage: workbench run <cmd...>"
      eval "$(print_env)"
      exec "$@"
      ;;
    status) wb_init; cmd_status ;;
    clean) wb_init; cmd_clean "$@" ;;
    ""|-h|--help|help) sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//' ;;
    *) die "unknown command: $cmd (try: workbench help)" ;;
  esac
}

main "$@"
