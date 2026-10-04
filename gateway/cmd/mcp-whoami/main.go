// Command mcp-whoami is the smoke helper for the worker MCP channel.
// Usage: go run ./gateway/cmd/mcp-whoami <data-dir> <worker-api-base>
// It registers a local worker through the control plane, seals a whoami
// call, and checks the reply names that worker.
package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"piso/internal/mcpchannel"
	tc "piso/internal/technocore"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: mcp-whoami <data-dir> <worker-api-base>")
		os.Exit(2)
	}
	dataDir, base := os.Args[1], os.Args[2]
	id, err := tc.Load(tc.Path(dataDir))
	if err != nil {
		die("load gateway identity: %v", err)
	}
	gwPub, err := hex.DecodeString(id.EncryptionPublicKey)
	if err != nil {
		die("gateway key: %v", err)
	}
	if err := registerSmoke(base); err != nil {
		die("register worker: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		die("worker key: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "whoami", "arguments": map[string]any{}},
	})
	sealed, ctx, err := mcpchannel.SealRequest(gwPub, pub, priv, body, time.Now())
	if err != nil {
		die("seal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/mcp", bytes.NewReader(sealed))
	if err != nil {
		die("request: %v", err)
	}
	req.Header.Set("Content-Type", mcpchannel.ContentType)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		die("post: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		die("mcp HTTP %d (%d bytes)", res.StatusCode, len(raw))
	}
	opened, err := ctx.OpenResponse(raw)
	if err != nil {
		die("open response: %v", err)
	}
	if !bytes.Contains(opened, []byte(`"slug":"smoke"`)) && !bytes.Contains(opened, []byte(`\"slug\":\"smoke\"`)) {
		die("whoami missing slug: %s", opened)
	}
	fmt.Println("whoami ok")
}

// registerSmoke posts the loopback worker to the control plane. The gateway
// does not reload state.json, so writing the file from a second store would
// be invisible to the running process.
func registerSmoke(workerBase string) error {
	ctrl := strings.Replace(workerBase, ":18083", ":18081", 1)
	ctrl = strings.Replace(ctrl, ":18183", ":18181", 1)
	body, _ := json.Marshal(map[string]any{
		"name": "piso-worker-smoke", "slug": "smoke", "ips": []string{"127.0.0.1"},
	})
	res, err := http.Post(ctrl+"/api/v1/workers", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, raw)
	}
	return nil
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mcp-whoami: "+format+"\n", args...)
	os.Exit(1)
}
