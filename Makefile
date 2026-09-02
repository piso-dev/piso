.PHONY: build test gw-run smoke isolation install

BIN ?= bin
# Default matches /usr/local/bin on PATH. Use PREFIX=$$HOME/.local to avoid sudo,
# or PREFIX=/opt/homebrew on Apple Silicon Homebrew.
PREFIX ?= /usr/local
SHARE  := $(PREFIX)/share/piso

build:
	go build -o $(BIN)/piso ./cli/cmd/piso
	go build -o $(BIN)/gateway ./gateway/cmd/gateway

# Install a global `piso` and the Docker build context it needs:
#   $(PREFIX)/bin/piso
#   $(PREFIX)/share/piso/{compose,worker,gateway,go.mod}
# The binary finds share/piso via ../share/piso relative to itself, so it
# works from any cwd. Gateway state lives in ~/.piso (PISO_DATA).
install: build
	install -d $(PREFIX)/bin $(SHARE)
	install -m 755 $(BIN)/piso $(PREFIX)/bin/piso
	rm -rf $(SHARE)/compose $(SHARE)/worker $(SHARE)/gateway
	cp -R compose worker gateway $(SHARE)/
	install -m 644 go.mod $(SHARE)/go.mod
	if [ -f .dockerignore ]; then install -m 644 .dockerignore $(SHARE)/.dockerignore; fi
	@echo "installed $(PREFIX)/bin/piso"
	@echo "share     $(SHARE)"
	@echo "data      \$$HOME/.piso  (created on first piso up)"

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