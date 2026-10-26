#!/bin/sh
# go shim: use the host toolchain when there is one, otherwise build in the same
# golang image the gateway image already uses (gateway/Dockerfile). These hosts
# often have no Go — the coordinator may well run inside a container — and
# `make install` would otherwise die at `go mod tidy` before Docker does
# anything useful.
#
# Container mode mounts the repo at /src, keeps the caller's uid/gid so bin/ and
# go.sum are not left root-owned, and caches modules/builds under
# .tools/go-cache (gitignored) instead of re-downloading on every invocation.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

if command -v go >/dev/null 2>&1; then
	exec go "$@"
fi

if ! command -v docker >/dev/null 2>&1; then
	echo "go: no Go toolchain, and docker is not on PATH either." >&2
	echo "go: install Go (https://go.dev/dl/) or make docker reachable, then retry." >&2
	exit 1
fi

image=${PISO_GO_IMAGE:-golang:1.25-alpine}
cache=${PISO_GO_CACHE:-$root/.tools/go-cache}
mkdir -p "$cache/mod" "$cache/build"

# GOOS/GOARCH/GOFLAGS are passed through bare (`-e NAME`) so a caller's
# cross-compile environment is forwarded without this script re-parsing it.
exec docker run --rm \
	--user "$(id -u):$(id -g)" \
	-e GOOS -e GOARCH -e GOFLAGS \
	-e GOTOOLCHAIN=local \
	-e GOMODCACHE=/cache/mod \
	-e GOCACHE=/cache/build \
	-e HOME=/tmp \
	-e CGO_ENABLED=0 \
	-v "$root":/src \
	-v "$cache":/cache \
	-w /src \
	"$image" go "$@"
