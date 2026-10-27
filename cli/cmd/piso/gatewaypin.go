package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"piso/internal/mcpchannel"
)

// A worker cannot talk to the gateway without the gateway's MCP public pin:
// piso-mcp reads it at startup and exits "gateway pin missing" before it sends
// a single byte (cmd/piso-mcp/main.go). The pin is public material published at
// GET /mcp/key, and the worker container reads it from the directory mounted at
// /etc/piso, which compose binds to <data>/workers/<slug>.
//
// The gateway writes that file itself for its own workers, but a remote ship is
// provisioned by the CLI on a different machine: pairing hands back ca_pem and
// worker_token only, so nothing ever created gateway.json there. An enrolled
// ship then looks healthy - check-in is plain HTTP with the token and keeps
// working - while every piso-mcp call fails and task-watch spins
// "peek failed (retry in 30s)". Fetch the pin at launch, once the gateway CA is
// already trusted, so the channel has what it needs before the container starts.

// gatewayPinMountPath is where the worker container expects the pin.
const gatewayPinMountPath = "/etc/piso/gateway.json"

// maxGatewayPinBytes bounds GET /mcp/key. A pin is a few hundred bytes; the
// limit keeps a wrong URL (an HTML error page, a log dump) out of the file.
const maxGatewayPinBytes = 8 << 10

// pinInstall reports what installWorkerGatewayPin did, so the caller can print
// the keyId and say whether an existing pin was replaced.
type pinInstall struct {
	Pin         mcpchannel.Pin
	Fingerprint string // gateway identity fingerprint, when the response has one
	Path        string
	Replaced    string // keyId that was on disk before; "" when none
	Changed     bool
}

// fetchGatewayPin reads the gateway's public pin from GET /mcp/key.
//
// It uses pairHTTP(), the client the pairing flow already settled trust for:
// same host, same CA, same channel that just returned the worker assignment.
// The response is not authenticated beyond TLS, which is the same trust anchor
// /ca.crt was fetched under - and the pin is worthless to an attacker unless
// they also hold the matching private keys.
func fetchGatewayPin(serverURL string) (mcpchannel.Pin, string, error) {
	base := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if base == "" {
		return mcpchannel.Pin{}, "", errors.New("gateway pin: no gateway endpoint to fetch from")
	}
	url := base + "/mcp/key"
	res, err := pairHTTP().Get(url)
	if err != nil {
		return mcpchannel.Pin{}, "", fmt.Errorf("gateway pin: %s: %w", url, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxGatewayPinBytes+1))
	if err != nil {
		return mcpchannel.Pin{}, "", fmt.Errorf("gateway pin: %s: %w", url, err)
	}
	if res.StatusCode != http.StatusOK {
		return mcpchannel.Pin{}, "", fmt.Errorf("gateway pin: %s returned %s", url, res.Status)
	}
	if len(raw) > maxGatewayPinBytes {
		return mcpchannel.Pin{}, "", fmt.Errorf("gateway pin: %s returned more than %d bytes", url, maxGatewayPinBytes)
	}
	pin, err := mcpchannel.ParsePin(raw)
	if err != nil {
		return mcpchannel.Pin{}, "", fmt.Errorf("gateway pin: %s: %w", url, err)
	}
	var meta struct {
		Fingerprint string `json:"fingerprint"`
	}
	_ = json.Unmarshal(raw, &meta) // informational only; never required
	return pin, meta.Fingerprint, nil
}

// installWorkerGatewayPin fetches the pin and writes it where the worker
// container will read it. An unchanged pin is left alone, so a relaunch does
// not rewrite the file under a running container.
func installWorkerGatewayPin(serverURL, slugDir string) (pinInstall, error) {
	out := pinInstall{Path: filepath.Join(slugDir, mcpchannel.PinFile)}
	if prev, err := mcpchannel.LoadPin(out.Path); err == nil {
		out.Replaced = prev.KeyID
	}
	pin, fp, err := fetchGatewayPin(serverURL)
	if err != nil {
		return out, err
	}
	out.Pin, out.Fingerprint = pin, fp
	if out.Replaced == pin.KeyID {
		return out, nil // already pinned to this gateway; nothing to write
	}
	if err := mcpchannel.WritePin(out.Path, pin); err != nil {
		return out, err
	}
	out.Changed = true
	return out, nil
}
