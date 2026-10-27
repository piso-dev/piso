package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
// The pin itself is public material, but on a public gateway the route sits
// behind the remote-worker allowlist (gateway/internal/server/public.go), which
// admits only a valid worker bearer token - IP allowlisting excludes remote
// workers on purpose. Anonymous fetch therefore gets 401, which is how a ship
// ended up with no pin at all: the first fix forgot the token.
func fetchGatewayPin(serverURL, token string) (mcpchannel.Pin, string, error) {
	base := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if base == "" {
		return mcpchannel.Pin{}, "", errors.New("gateway pin: no gateway endpoint to fetch from")
	}
	url := base + "/mcp/key"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return mcpchannel.Pin{}, "", fmt.Errorf("gateway pin: %s: %w", url, err)
	}
	if token = strings.TrimSpace(token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := pairHTTP().Do(req)
	if err != nil {
		return mcpchannel.Pin{}, "", fmt.Errorf("gateway pin: %s: %w", url, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxGatewayPinBytes+1))
	if err != nil {
		return mcpchannel.Pin{}, "", fmt.Errorf("gateway pin: %s: %w", url, err)
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusProxyAuthRequired {
		return mcpchannel.Pin{}, "", fmt.Errorf("gateway pin: %s rejected this worker's token (%s); the gateway must know it as a worker", url, res.Status)
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

// installAssignedWorkerGatewayPin trusts the assigned gateway, which may be a
// separate TLS host from the coordinator used during pairing.
func installAssignedWorkerGatewayPin(asg workerAssignment, slugDir string) (pinInstall, error) {
	previous := pairClient
	defer func() { pairClient = previous }()
	if asg.CAPem != "" {
		pool, _, _, err := loadCAPool([]byte(asg.CAPem), "assigned gateway CA")
		if err != nil {
			return pinInstall{}, err
		}
		setPairClient(&gatewayCA{client: poolClient(pool)})
	}
	return installWorkerGatewayPin(asg.Endpoint, asg.Token, slugDir)
}

// installWorkerGatewayPin fetches the pin and writes it where the worker
// container will read it. An unchanged pin is left alone, so a relaunch does
// not rewrite the file under a running container.
func installWorkerGatewayPin(serverURL, token, slugDir string) (pinInstall, error) {
	out := pinInstall{Path: filepath.Join(slugDir, mcpchannel.PinFile)}
	if prev, err := mcpchannel.LoadPin(out.Path); err == nil {
		out.Replaced = prev.KeyID
	}
	if token = strings.TrimSpace(token); token == "" {
		// A relaunch may carry the assignment without the token; the ship keeps
		// the same value in <data>/workers/<slug>/token, which compose also
		// mounts at /etc/piso/token for the container.
		if raw, err := os.ReadFile(filepath.Join(slugDir, "token")); err == nil {
			token = strings.TrimSpace(string(raw))
		}
	}
	pin, fp, err := fetchGatewayPin(serverURL, token)
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
