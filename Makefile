.PHONY: build build-piso deps share-copy gw-run smoke isolation install install-share install-share-worker install-worker setup-install uninstall windows

BIN ?= bin
# Default matches /usr/local/bin on PATH. Use PREFIX=$$HOME/.local to avoid sudo,
# or PREFIX=/opt/homebrew on Apple Silicon Homebrew.
PREFIX ?= /usr/local
SHARE  := $(PREFIX)/share/piso

# sudo make install must not write $HOME as root (HOME=/var/root) or talk to
# Docker as root (Desktop/OrbStack sockets belong to the login user).
REAL_USER := $(if $(SUDO_USER),$(SUDO_USER),$(USER))
REAL_HOME := $(shell eval echo ~$(REAL_USER))
ifeq ($(REAL_HOME),)
REAL_HOME := $(HOME)
endif
ifneq ($(SUDO_USER),)
AS_USER := sudo -u $(SUDO_USER) -H
else
AS_USER :=
endif
# sudo resets PATH; keep the places go / docker usually live on macOS.
USER_PATH := $(REAL_HOME)/.orbstack/bin:/usr/local/go/bin:/usr/local/bin:/opt/homebrew/bin:$(PATH)
# A go binary, or a shim that builds in the golang image when the host has none
# (containers, CI). scripts/go.sh prefers a real toolchain when one appears.
GO := $(shell command -v go 2>/dev/null || echo sh $(CURDIR)/scripts/go.sh)

build: build-piso
	$(AS_USER) env HOME="$(REAL_HOME)" PATH="$(USER_PATH)" $(GO) build -o $(BIN)/gateway ./gateway/cmd/gateway

# The CLI alone. A cloud ship needs only this: nothing on the ship runs
# bin/gateway (the gateway there is a container on another machine), and the
# gateway build is by far the expensive half — modernc.org/sqlite's generated
# package needs ~3GB to compile one file and is the usual `signal: killed` on a
# small VM.
build-piso: deps
	$(AS_USER) env HOME="$(REAL_HOME)" PATH="$(USER_PATH)" $(GO) build -o $(BIN)/piso ./cli/cmd/piso

# First run after adding an external module (e.g. modernc.org/sqlite): resolve
# the graph + write go.sum on the host (this repo has no Go toolchain inside the
# container). Idempotent when clean, and it does not compile anything.
deps:
	$(AS_USER) env HOME="$(REAL_HOME)" PATH="$(USER_PATH)" $(GO) mod tidy

# Cross-compile the Windows CLI. The gateway stays a Linux image.
windows:
	GOOS=windows GOARCH=amd64 $(GO) build -o $(BIN)/piso.exe ./cli/cmd/piso

# Install (or replace) a global `piso` and the Docker build context it needs:
#   $(PREFIX)/bin/piso
#   $(PREFIX)/share/piso/{compose,worker,gateway,internal,go.mod}
# Safe to re-run over an existing install: binaries and share files are
# overwritten, then `piso setup --rebuild` recreates the gateway so the new
# image loads ~/.piso/state.json and migrates it. When invoked via sudo,
# setup runs as $(SUDO_USER) so data stays in that user's ~/.piso.
install: install-share
	@echo "installed $(PREFIX)/bin/piso"
	@echo "share     $(SHARE)"
	@echo "data      $(REAL_HOME)/.piso"
	$(MAKE) setup-install
	@echo "restarting hosts sync daemon with the new binary"
	PISO_DATA="$(REAL_HOME)/.piso" $(PREFIX)/bin/piso sync daemon-restart || \
		echo "piso: warning: sync daemon did not verify up (see above); retry: sudo PISO_DATA=$(REAL_HOME)/.piso $(PREFIX)/bin/piso sync daemon-restart"

# The file copy both install flavors share: binary + the whole share tree.
# `piso` finds this tree via Home() (compose/gateway.yaml must exist) and
# `piso worker launch` stages a build context from worker/, internal/ and
# cmd/, so the tree is copied whole even on a cloud ship — only the
# PRE-requisite (which binary is built) and the POST-install differ.
share-copy: build-piso
	install -d $(PREFIX)/bin $(SHARE)
	install -m 755 $(BIN)/piso $(PREFIX)/bin/piso
	rm -rf $(SHARE)/compose $(SHARE)/worker $(SHARE)/gateway $(SHARE)/internal $(SHARE)/cmd $(SHARE)/third_party
	cp -R compose worker gateway internal cmd $(SHARE)/
	install -m 644 go.mod $(SHARE)/go.mod
	install -m 644 go.sum $(SHARE)/go.sum
	if [ -f .dockerignore ]; then install -m 644 .dockerignore $(SHARE)/.dockerignore; fi

# Full install: bin/gateway is only used by scripts/smoke.sh, but a gateway
# host is where someone runs it.
install-share: build share-copy

# Cloud-ship install: the CLI only, so a 2GB box can build it.
install-share-worker: share-copy

# Cloud-ship install: a worker-only box (its own machine, never the standalone
# gateway host), so skip every gateway-host side effect of `make install`:
#   - no `piso setup --rebuild` (that builds the gateway image and runs a
#     piso-gateway + project-manager monitor container on this box)
#   - no hosts-sync daemon (nobody browses *.piso.local from the ship; ingress
#     is reached from the laptop through the assigned gateway)
# It still persists PISO_MONITOR=false, so a later gateway build here cannot
# resurrect a monitor worker. The worker image bake (apt/bun/pi/npm, minutes)
# is unchanged: only prebuilt images from CI remove that.
install-worker: install-share-worker
	@echo "installed $(PREFIX)/bin/piso (cloud ship: no gateway, no monitor, no hosts daemon)"
	@echo "share     $(SHARE)"
	@echo "data      $(REAL_HOME)/.piso"
	$(AS_USER) env HOME="$(REAL_HOME)" PATH="$(USER_PATH)" \
		PISO_DATA="$(REAL_HOME)/.piso" \
		$(PREFIX)/bin/piso setup --no-monitor
	@echo "next: piso worker launch --server https://<gateway>   (no dir needed: it mounts ~/piso-work; --name to override the hostname)"

# Uninstall: stop the global hosts-sync daemon first, then remove binaries and
# the share tree. User data (~/.piso: secrets, CA, state) is intentionally kept.
uninstall:
	@echo "stopping hosts sync daemon"
	-PISO_DATA="$(REAL_HOME)/.piso" $(PREFIX)/bin/piso sync daemon-uninstall || true
	rm -f $(PREFIX)/bin/piso
	rm -rf $(SHARE)
	@echo "piso: uninstalled (kept $(REAL_HOME)/.piso data)"

# Post-install as the login user: import leftover repo .piso files that are
# missing from ~/.piso, then rebuild/recreate the gateway container.
setup-install:
	$(AS_USER) env HOME="$(REAL_HOME)" PATH="$(USER_PATH)" \
		PISO_DATA="$(REAL_HOME)/.piso" \
		$(if $(wildcard .piso/state.json),PISO_MIGRATE_FROM="$(abspath .piso)") \
		$(PREFIX)/bin/piso setup --rebuild

vet:
	go vet ./...

# run the gateway locally (no docker) for development
gw-run:
	go run ./gateway/cmd/gateway -state .piso/state.json -patterns .piso/patterns.json -log .piso/requests.jsonl

smoke:
	./scripts/smoke.sh

# Docker-level: worker noproxy must fail; proxy + gateway egress must work.
isolation:
	./scripts/isolation.sh

clean:
	rm -rf $(BIN) .piso

# Test targets live in a separate fragment so the public mirror can ship its
# own suite (public/Makefile.tests) without carrying private dependencies.
include Makefile.tests
