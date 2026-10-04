package mcpchannel

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudflare/circl/hpke"
)

// TestWriteGolden regenerates testdata/golden.json when PISO_WRITE_GOLDEN=1.
// The committed vector is opened by TestGoldenVector.
func TestWriteGolden(t *testing.T) {
	if os.Getenv("PISO_WRITE_GOLDEN") != "1" {
		t.Skip("set PISO_WRITE_GOLDEN=1 to regenerate")
	}
	// Fixed seeds so the file is stable if regenerated from the same keys.
	// Keys below are generated once and then hardcoded by the first run's output.
	_, sk, err := hpke.KEM_X25519_HKDF_SHA256.Scheme().GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	gwPriv, _ := sk.MarshalBinary()
	gwPub, _ := sk.Public().MarshalBinary()
	wPub, wPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	// Deterministic nonce/enc are whatever circl and crypto/rand produce.
	// The test records them; later runs only open the file.
	body, _, err := SealRequest(gwPub, wPub, wPriv, content, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	req, err := OpenRequest(gwPriv, body)
	if err != nil {
		t.Fatal(err)
	}
	respPT := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	resp, err := req.SealResponse(respPT)
	if err != nil {
		t.Fatal(err)
	}
	seed := wPriv.Seed()
	out := map[string]any{
		"gatewayPub":         hex.EncodeToString(gwPub),
		"gatewayPriv":        hex.EncodeToString(gwPriv),
		"workerPub":          hex.EncodeToString(wPub),
		"workerPriv":         hex.EncodeToString(seed),
		"content":            string(content),
		"ts":                 req.TS,
		"nonce":              hex.EncodeToString(req.Nonce),
		"enc":                hex.EncodeToString(req.Enc),
		"request":            hex.EncodeToString(body),
		"response":           hex.EncodeToString(resp),
		"responsePlaintext":  string(respPT),
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	path := filepath.Join("testdata", "golden.json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", path)
}
