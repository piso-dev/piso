package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"piso/gateway/internal/patterns"
	"piso/gateway/internal/store"
	"piso/internal/mcpchannel"
	tc "piso/internal/technocore"

	"github.com/cloudflare/circl/hpke"
)

func mcpFixture(t *testing.T) (*Server, tc.Identity, []byte, []byte) {
	t.Helper()
	s := testServer(t)
	id, err := tc.Generate("", "gw")
	if err != nil {
		t.Fatal(err)
	}
	id.Status = tc.StatusActive
	s.DataDir = t.TempDir()
	if err := tc.Save(tc.Path(s.DataDir), id); err != nil {
		t.Fatal(err)
	}
	pat, err := patterns.Load("")
	if err != nil {
		t.Fatal(err)
	}
	s.Patterns = pat
	gwPub, _ := hex.DecodeString(id.EncryptionPublicKey)
	gwPriv, _ := hex.DecodeString(id.EncryptionPrivateKey)
	return s, id, gwPub, gwPriv
}

func registerLocal(t *testing.T, s *Server, ip string) store.WorkerRec {
	t.Helper()
	rec, err := s.Store.UpsertWorker(store.WorkerRec{
		Name: "piso-worker-demo", Slug: "demo", IPs: []string{ip},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func sealCall(t *testing.T, gwPub []byte, wPub ed25519.PublicKey, wPriv ed25519.PrivateKey, method string, params any, ts time.Time) ([]byte, *mcpchannel.ResponseContext) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	sealed, ctx, err := mcpchannel.SealRequest(gwPub, wPub, wPriv, raw, ts)
	if err != nil {
		t.Fatal(err)
	}
	return sealed, ctx
}

func postMCP(t *testing.T, h http.Handler, ip string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.RemoteAddr = ip + ":1234"
	req.Header.Set("Content-Type", mcpchannel.ContentType)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func workerKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestMCPPreDecryptBareStatus(t *testing.T) {
	s, _, gwPub, _ := mcpFixture(t)
	h := s.WorkerHandler()
	now := time.Now()

	w := postMCP(t, h, "192.168.107.50", []byte{0x02, 0, 0, 0, 0, 0, 0, 0, 0}, nil)
	if w.Code != 400 || w.Body.Len() != 0 {
		t.Fatalf("bad version: %d body %q", w.Code, w.Body.String())
	}

	short := []byte{0x01}
	w = postMCP(t, h, "192.168.107.50", short, nil)
	if w.Code != 400 || w.Body.Len() != 0 {
		t.Fatalf("short: %d %q", w.Code, w.Body.String())
	}

	pub, priv := workerKeys(t)
	sealed, _ := sealCall(t, gwPub, pub, priv, "ping", nil, now)
	sealed[1] ^= 0xff
	w = postMCP(t, h, "192.168.107.50", sealed, nil)
	if w.Code != 421 || w.Body.Len() != 0 {
		t.Fatalf("wrong key id: %d %q", w.Code, w.Body.String())
	}

	sealed, _ = sealCall(t, gwPub, pub, priv, "ping", nil, now)
	sealed[len(sealed)-1] ^= 0x01
	w = postMCP(t, h, "192.168.107.50", sealed, nil)
	if w.Code != 400 || w.Body.Len() != 0 {
		t.Fatalf("open fail: %d %q", w.Code, w.Body.String())
	}

	huge := bytes.Repeat([]byte{0x01}, mcpchannel.MaxRequest+1)
	w = postMCP(t, h, "192.168.107.50", huge, nil)
	if w.Code != http.StatusRequestEntityTooLarge || w.Body.Len() != 0 {
		t.Fatalf("cap: %d %q", w.Code, w.Body.String())
	}
}

func TestMCPTrustOnFirstUseAndReplay(t *testing.T) {
	s, id, gwPub, _ := mcpFixture(t)
	registerLocal(t, s, "192.168.107.50")
	h := s.WorkerHandler()
	pub, priv := workerKeys(t)
	now := time.Now()
	sealed, ctx := sealCall(t, gwPub, pub, priv, "tools/call", map[string]any{
		"name": "whoami", "arguments": map[string]any{"worker": "piso-worker-other"},
	}, now)
	w := postMCP(t, h, "192.168.107.50", sealed, nil)
	if w.Code != 200 {
		t.Fatalf("first: %d %q", w.Code, w.Body.String())
	}
	pt, err := ctx.OpenResponse(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pt), `\"slug\":\"demo\"`) && !strings.Contains(string(pt), `"slug":"demo"`) {
		t.Fatalf("caller not envelope: %s", pt)
	}
	if strings.Contains(string(pt), "piso-worker-other") {
		t.Fatalf("arguments chose the caller: %s", pt)
	}
	if !strings.Contains(string(pt), id.Fingerprint()) {
		t.Fatalf("missing gateway fingerprint: %s", pt)
	}
	bound, ok := s.Store.WorkerByName("piso-worker-demo")
	if !ok || !strings.EqualFold(bound.ChannelPubkey, hex.EncodeToString(pub)) {
		t.Fatalf("not bound: %+v", bound)
	}

	// Replay of the same sealed body is rejected with an empty 409.
	w = postMCP(t, h, "192.168.107.50", sealed, nil)
	if w.Code != 409 || w.Body.Len() != 0 {
		t.Fatalf("replay: %d %q", w.Code, w.Body.String())
	}

	// A second key from the same IP is 401 and raises an inbox row.
	pub2, priv2 := workerKeys(t)
	sealed2, _ := sealCall(t, gwPub, pub2, priv2, "ping", nil, now.Add(time.Second))
	w = postMCP(t, h, "192.168.107.50", sealed2, nil)
	if w.Code != 401 || w.Body.Len() != 0 {
		t.Fatalf("second key: %d %q", w.Code, w.Body.String())
	}
	pending := s.Store.PendingIngress()
	if len(pending) != 1 || pending[0].Kind != store.IngressKindRebind {
		t.Fatalf("inbox %+v", pending)
	}

	// Wrong source IP for the bound key.
	sealed3, _ := sealCall(t, gwPub, pub, priv, "ping", nil, now.Add(2*time.Second))
	w = postMCP(t, h, "192.168.107.99", sealed3, nil)
	if w.Code != 401 || w.Body.Len() != 0 {
		t.Fatalf("wrong ip: %d %q", w.Code, w.Body.String())
	}
}

func TestMCPStaleTimestamp(t *testing.T) {
	s, _, gwPub, _ := mcpFixture(t)
	registerLocal(t, s, "192.168.107.50")
	pub, priv := workerKeys(t)
	sealed, _ := sealCall(t, gwPub, pub, priv, "ping", nil, time.Now().Add(-2*time.Minute))
	w := postMCP(t, s.WorkerHandler(), "192.168.107.50", sealed, nil)
	if w.Code != 409 || w.Body.Len() != 0 {
		t.Fatalf("stale: %d %q", w.Code, w.Body.String())
	}
}

func TestMCPScannerBlocksWriteTool(t *testing.T) {
	s, _, gwPub, _ := mcpFixture(t)
	registerLocal(t, s, "192.168.107.50")
	pub, priv := workerKeys(t)
	sealed, ctx := sealCall(t, gwPub, pub, priv, "tools/call", map[string]any{
		"name": "mcp_add",
		"arguments": map[string]any{
			"url":  "https://example.com/mcp",
			"name": "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789",
		},
	}, time.Now())
	w := postMCP(t, s.WorkerHandler(), "192.168.107.50", sealed, nil)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	pt, err := ctx.OpenResponse(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pt), "credential pattern blocked") {
		t.Fatalf("not blocked: %s", pt)
	}
	if _, ok := s.Store.FindMcp("piso-worker-demo", "example.com", "https://example.com/mcp"); ok {
		t.Fatal("write tool ran")
	}
}

func TestMCPApproveRebind(t *testing.T) {
	s, _, gwPub, _ := mcpFixture(t)
	registerLocal(t, s, "192.168.107.50")
	pub, priv := workerKeys(t)
	now := time.Now()
	sealed, _ := sealCall(t, gwPub, pub, priv, "ping", nil, now)
	if w := postMCP(t, s.WorkerHandler(), "192.168.107.50", sealed, nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	pub2, priv2 := workerKeys(t)
	sealed2, _ := sealCall(t, gwPub, pub2, priv2, "ping", nil, now.Add(time.Second))
	if w := postMCP(t, s.WorkerHandler(), "192.168.107.50", sealed2, nil); w.Code != 401 {
		t.Fatalf("second %d", w.Code)
	}
	pending := s.Store.PendingIngress()
	if len(pending) != 1 {
		t.Fatal(pending)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingress/requests/"+pending[0].ID+"/approve", nil)
	rec := httptest.NewRecorder()
	s.ControlHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("approve %d %s", rec.Code, rec.Body.String())
	}
	sealed3, ctx := sealCall(t, gwPub, pub2, priv2, "ping", nil, now.Add(2*time.Second))
	w := postMCP(t, s.WorkerHandler(), "192.168.107.50", sealed3, nil)
	if w.Code != 200 {
		t.Fatalf("after approve %d", w.Code)
	}
	pt, err := ctx.OpenResponse(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pt), `"result"`) {
		t.Fatalf("ping %s", pt)
	}
}

func TestMCPKeyEndpoint(t *testing.T) {
	s, id, _, _ := mcpFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp/key", nil)
	w := httptest.NewRecorder()
	s.WorkerHandler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var pin mcpchannel.Pin
	if err := json.Unmarshal(w.Body.Bytes(), &pin); err != nil {
		t.Fatal(err)
	}
	if err := pin.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(pin.Signing[:12], id.Fingerprint()) {
		t.Fatalf("signing %s fingerprint %s", pin.Signing, id.Fingerprint())
	}
	_, _ = io.ReadAll(w.Body)
}

func TestSignedForOtherGatewayBare401(t *testing.T) {
	s, _, _, _ := mcpFixture(t)
	registerLocal(t, s, "192.168.107.50")
	_, sk, err := hpke.KEM_X25519_HKDF_SHA256.Scheme().GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	other, err := sk.Public().MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	pub, priv := workerKeys(t)
	// Sealed to a different gateway: our key id check returns 421 before open.
	sealed, _ := sealCall(t, other, pub, priv, "ping", nil, time.Now())
	w := postMCP(t, s.WorkerHandler(), "192.168.107.50", sealed, nil)
	if w.Code != 421 || w.Body.Len() != 0 {
		t.Fatalf("other gateway: %d %q", w.Code, w.Body.String())
	}
}
