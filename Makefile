.PHONY: build test gw-run smoke isolation install setup-install uninstall windows

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

build:
	# First run after adding an external module (e.g. modernc.org/sqlite):
	# resolve the graph + write go.sum on the host (this repo has no Go
	# toolchain inside the container). Idempotent when clean.
	$(AS_USER) env HOME="$(REAL_HOME)" PATH="$(USER_PATH)" go mod tidy
	$(AS_USER) env HOME="$(REAL_HOME)" PATH="$(USER_PATH)" go build -o $(BIN)/piso ./cli/cmd/piso
	$(AS_USER) env HOME="$(REAL_HOME)" PATH="$(USER_PATH)" go build -o $(BIN)/gateway ./gateway/cmd/gateway

# Cross-compile the Windows CLI. The gateway stays a Linux image.
windows:
	GOOS=windows GOARCH=amd64 go build -o $(BIN)/piso.exe ./cli/cmd/piso

# Install (or replace) a global `piso` and the Docker build context it needs:
#   $(PREFIX)/bin/piso
#   $(PREFIX)/share/piso/{compose,worker,gateway,go.mod}
# Safe to re-run over an existing install: binaries and share files are
# overwritten, then `piso setup --rebuild` recreates the gateway so the new
# image loads ~/.piso/state.json and migrates it. When invoked via sudo,
# setup runs as $(SUDO_USER) so data stays in that user's ~/.piso.
install: build
	install -d $(PREFIX)/bin $(SHARE)
	install -m 755 $(BIN)/piso $(PREFIX)/bin/piso
	rm -rf $(SHARE)/compose $(SHARE)/worker $(SHARE)/gateway
	cp -R compose worker gateway $(SHARE)/
	install -m 644 go.mod $(SHARE)/go.mod
	install -m 644 go.sum $(SHARE)/go.sum
	if [ -f .dockerignore ]; then install -m 644 .dockerignore $(SHARE)/.dockerignore; fi
	@echo "installed $(PREFIX)/bin/piso"
	@echo "share     $(SHARE)"
	@echo "data      $(REAL_HOME)/.piso"
	$(MAKE) setup-install
	@echo "restarting hosts sync daemon with the new binary"
	PISO_DATA="$(REAL_HOME)/.piso" $(PREFIX)/bin/piso sync daemon-restart || \
		echo "piso: warning: sync daemon did not verify up (see above); retry: sudo PISO_DATA=$(REAL_HOME)/.piso $(PREFIX)/bin/piso sync daemon-restart"

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

test:
	go test ./...

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
