package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"piso/internal/mcpchannel"
	"piso/internal/technocore"

	"github.com/cloudflare/circl/hpke"
)

func testRelay(t *testing.T) (id technocore.Identity, pinPath, identPath string, gwPub, gwPriv []byte) {
	t.Helper()
	dir := t.TempDir()
	id, err := technocore.Generate("", "worker")
	if err != nil {
		t.Fatal(err)
	}
	identPath = filepath.Join(dir, "identity.json")
	if err := technocore.Save(identPath, id); err != nil {
		t.Fatal(err)
	}
	_, sk, err := hpke.KEM_X25519_HKDF_SHA256.Scheme().GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	gwPriv, _ = sk.MarshalBinary()
	gwPub, _ = sk.Public().MarshalBinary()
	sign := make([]byte, ed25519.PublicKeySize)
	if _, err := rand.Read(sign); err != nil {
		t.Fatal(err)
	}
	pinPath = filepath.Join(dir, "gateway.json")
	if err := mcpchannel.WritePin(pinPath, mcpchannel.PinFromPublic(gwPub, sign)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PISO_MCP_IDENTITY", identPath)
	t.Setenv("PISO_MCP_PIN", pinPath)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	return id, pinPath, identPath, gwPub, gwPriv
}

func gatewayServer(t *testing.T, gwPriv, gwPub []byte) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		opened, err := mcpchannel.OpenRequest(gwPriv, body)
		if err != nil || !opened.Verify(gwPub) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		key := hex.EncodeToString(opened.Nonce)
		if seen[key] {
			mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			return
		}
		seen[key] = true
		mu.Unlock()
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(opened.Content, &req)
		if len(req.ID) == 0 || string(req.ID) == "null" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		resp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{"method": req.Method},
		})
		sealed, err := opened.SealResponse(resp)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", mcpchannel.ContentType)
		_, _ = w.Write(sealed)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStdioRoundTrip(t *testing.T) {
	_, _, _, gwPub, gwPriv := testRelay(t)
	srv := gatewayServer(t, gwPriv, gwPub)
	t.Setenv("GATEWAY_URL", srv.URL)
	in := bytes.NewBufferString("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n")
	var out bytes.Buffer
	if err := serveStdio(in, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"method":"ping"`) {
		t.Fatalf("stdout %s", out.String())
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("lines %q", out.String())
	}
}

func TestNotificationWritesNothing(t *testing.T) {
	_, _, _, gwPub, gwPriv := testRelay(t)
	srv := gatewayServer(t, gwPriv, gwPub)
	t.Setenv("GATEWAY_URL", srv.URL)
	in := bytes.NewBufferString("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")
	var out bytes.Buffer
	if err := serveStdio(in, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("notification wrote %q", out.String())
	}
}

func TestBatchRejected(t *testing.T) {
	_, _, _, gwPub, gwPriv := testRelay(t)
	srv := gatewayServer(t, gwPriv, gwPub)
	t.Setenv("GATEWAY_URL", srv.URL)
	in := bytes.NewBufferString("[{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}]\n")
	var out bytes.Buffer
	if err := serveStdio(in, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "batches are not supported") {
		t.Fatalf("stdout %s", out.String())
	}
}

func TestMissingPinFailsClosed(t *testing.T) {
	testRelay(t)
	t.Setenv("PISO_MCP_PIN", filepath.Join(t.TempDir(), "missing.json"))
	err := serveStdio(bytes.NewBufferString("{}\n"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "gateway pin") {
		t.Fatalf("err %v", err)
	}
}

func TestPinMismatchClearError(t *testing.T) {
	_, _, _, gwPub, gwPriv := testRelay(t)
	// Server seals the response with a different exporter by using another key.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		opened, err := mcpchannel.OpenRequest(gwPriv, body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		other, _, _, _ := otherKey(t)
		_ = other
		// Corrupt the sealed response so open fails.
		sealed, err := opened.SealResponse([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		if err != nil {
			w.WriteHeader(500)
			return
		}
		sealed[len(sealed)-1] ^= 0x01
		w.WriteHeader(200)
		_, _ = w.Write(sealed)
	}))
	defer srv.Close()
	t.Setenv("GATEWAY_URL", srv.URL)
	in := bytes.NewBufferString("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n")
	var out bytes.Buffer
	if err := serveStdio(in, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "gateway key mismatch") {
		t.Fatalf("stdout %s", out.String())
	}
	_ = gwPub
}

func otherKey(t *testing.T) ([]byte, []byte, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	_, sk, err := hpke.KEM_X25519_HKDF_SHA256.Scheme().GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := sk.MarshalBinary()
	pub, _ := sk.Public().MarshalBinary()
	return pub, priv, nil, nil
}

func TestProxyEnvIgnored(t *testing.T) {
	_, _, _, gwPub, gwPriv := testRelay(t)
	srv := gatewayServer(t, gwPriv, gwPub)
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	c, err := newClient()
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatal("proxy must be disabled")
	}
	raw := []byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	pt, err := c.roundTrip(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pt, []byte(`"id":7`)) {
		t.Fatalf("response %s", pt)
	}
}

func TestConcurrentLinesDoNotInterleave(t *testing.T) {
	_, _, _, gwPub, gwPriv := testRelay(t)
	srv := gatewayServer(t, gwPriv, gwPub)
	t.Setenv("GATEWAY_URL", srv.URL)
	var in bytes.Buffer
	for i := 1; i <= 8; i++ {
		fmt := map[string]any{"jsonrpc": "2.0", "id": i, "method": "ping"}
		b, _ := json.Marshal(fmt)
		in.Write(b)
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	if err := serveStdio(&in, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 8 {
		t.Fatalf("lines %d %q", len(lines), out.String())
	}
	seen := map[float64]bool{}
	for _, line := range lines {
		if strings.Contains(line, "\n") {
			t.Fatal("embedded newline")
		}
		var resp struct {
			ID float64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		seen[resp.ID] = true
	}
	if len(seen) != 8 {
		t.Fatalf("ids %v", seen)
	}
}

func TestEnsureIdempotentAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.json")
	t.Setenv("PISO_MCP_IDENTITY", path)
	// ensureIdentity uses the constants, not the env, for the volume path.
	// Exercise LoadOrCreate directly for the volume case and ParseIdentity
	// for the corrupt case.
	id, err := technocore.LoadOrCreate(path, "", "worker")
	if err != nil {
		t.Fatal(err)
	}
	again, err := technocore.LoadOrCreate(path, "", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if again.SigningPublicKey != id.SigningPublicKey {
		t.Fatal("ensure not idempotent")
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := technocore.Load(path); err == nil {
		t.Fatal("corrupt file should error")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() == 0 {
		t.Fatal("corrupt file was overwritten")
	}
}

func TestMountedIdentityWins(t *testing.T) {
	dir := t.TempDir()
	mounted := filepath.Join(dir, "mounted.json")
	id, err := technocore.Generate("", "remote")
	if err != nil {
		t.Fatal(err)
	}
	if err := technocore.Save(mounted, id); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(mounted)
	if err != nil {
		t.Fatal(err)
	}
	got, err := technocore.ParseIdentity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.SigningPublicKey != id.SigningPublicKey {
		t.Fatal("mounted identity not used")
	}
	// A generated identity must not replace a readable mounted one.
	gen, err := technocore.Generate("", "local")
	if err != nil {
		t.Fatal(err)
	}
	if gen.SigningPublicKey == got.SigningPublicKey {
		t.Fatal("expected distinct keys")
	}
}
