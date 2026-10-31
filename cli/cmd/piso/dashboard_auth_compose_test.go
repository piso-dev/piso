package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The gateway compose must not ship a trusted-peer default. That subnet also
// carries the host's own traffic to the published ports, so a default makes the
// dashboard gate accuse the operator's own machine of being off-host — it asks
// for a password nobody ever set, on http://piso.local. Gating a real front
// proxy is the operator's explicit act (export the value), not an inference.
func TestGatewayComposeHasNoTrustedPeerDefault(t *testing.T) {
	path := filepath.Join("..", "..", "..", "compose", "gateway.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// Match the service-env line, not the comments that mention it.
	line := regexp.MustCompile(`(?m)^\s*PISO_TRUSTED_PEER_CIDRS:\s*(\S.*)$`).FindStringSubmatch(string(raw))
	if line == nil {
		t.Fatalf("compose/gateway.yaml no longer passes PISO_TRUSTED_PEER_CIDRS to the gateway")
	}
	value := strings.TrimSpace(line[1])
	if want := "${PISO_TRUSTED_PEER_CIDRS:-}"; value != want {
		t.Fatalf("PISO_TRUSTED_PEER_CIDRS default = %q, want %q (a non-empty default gates the host's own dashboard)", value, want)
	}
}

// The coexist overlay cannot set the gateway's env — it is the `technocore`
// project, the gateway is `piso` — so it must tell the operator to export it.
func TestCoexistOverlayTellsOperatorToExportTrustedPeers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "server", "compose.coexist.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(raw), "export PISO_TRUSTED_PEER_CIDRS=") {
		t.Fatalf("server/compose.coexist.yaml must document exporting PISO_TRUSTED_PEER_CIDRS; Caddy is 403'd without it")
	}
}
