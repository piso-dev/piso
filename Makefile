.PHONY: build test gw-run smoke

BIN ?= bin

build:
	go build -o $(BIN)/piso ./cli/cmd/piso
	go build -o $(BIN)/gateway ./gateway/cmd/gateway

test:
	go test ./...

vet:
	go vet ./...

# run the gateway locally (no docker) for development
gw-run:
	go run ./gateway/cmd/gateway -state .piso/state.json -patterns .piso/patterns.json -log .piso/requests.jsonl

smoke:
	./scripts/smoke.sh

clean:
	rm -rf $(BIN) .piso