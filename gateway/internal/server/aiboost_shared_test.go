package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"strings"

	tc "piso/internal/technocore"
)

// sealedSharedConnection produces the coordinator objects list for one shared
// AI Boost envelope addressed to id, plus the plaintext for assertions.
func sealedSharedConnection(t *testing.T, id tc.Identity, baseURL, access string, accessExpires time.Time) (objects any, plain []byte) {
	t.Helper()
	plain, err := json.Marshal(map[string]any{"baseUrl": baseURL,
		"redirectUri": "http://piso.local/oauth/ai-boost/callback",
		"clientId":    "shared-client", "clientSecret": "shared-secret",
		"accessToken": access, "refreshToken": "shared-refresh",
		"accessExpires": accessExpires, "refreshExpires": time.Now().Add(time.Hour),
		"status": "ok"})
	if err != nil {
		t.Fatal(err)
	}
	dek, nonce, cipher, err := tc.SealPayload(plain)
	if err != nil {
		t.Fatal(err)
	}
	wrap, err := tc.WrapDEK(id.EncryptionPublicKey, dek)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(plain)
	objects = []any{map[string]any{
		"status": "active", "metadata": map[string]any{"kind": "ai-boost"},
		"content_version": 1, "content_hash": hex.EncodeToString(sum[:]),
		"ciphertext": base64.StdEncoding.EncodeToString(cipher),
		"nonce":      base64.StdEncoding.EncodeToString(nonce),
		"wrapped_data_key": base64.StdEncoding.EncodeToString(wrap),
	}}
	return objects, plain
}

func sharedCoordinator(t *testing.T, id tc.Identity, objects any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"objects": objects})
	}))
}

func requireNoSecretLeak(t *testing.T, s *Server, plain []byte) {
	t.Helper()
	for _, rec := range s.Store.Secrets() {
		if rec.Value == string(plain) {
			t.Fatal("shared AI Boost credentials entered the general vault")
		}
	}
}

// A gateway with no local vault imports the shared connection and serves a
// worker boost_get through the ordinary encrypted channel.
func TestBoostWorkerOnGatewayWithoutVault(t *testing.T) {
	s, _, gwPub, gwPriv := mcpFixture(t)
	id, err := tc.Load(tc.Path(s.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/library/boosters/22222222-2222-2222-2222-222222222222/" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer shared-access" {
			t.Errorf("bearer = %q", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "22222222-2222-2222-2222-222222222222", "name": "Shared",
			"version": "v1", "content": map[string]string{"format": "markdown", "body": "Shared guidance"}})
	}))
	defer upstream.Close()
	s.HTTP = upstream.Client()
	objects, plain := sealedSharedConnection(t, id, upstream.URL, "shared-access", time.Now().Add(10*time.Minute))
	coordinator := sharedCoordinator(t, id, objects)
	defer coordinator.Close()
	id.ServerURL = coordinator.URL
	if err := tc.Save(tc.Path(s.DataDir), id); err != nil {
		t.Fatal(err)
	}
	registerLocal(t, s, "192.168.107.51")
	workerPub, workerPriv := workerKeys(t)
	sealed, response := sealCall(t, gwPub, workerPub, workerPriv, "tools/call", map[string]any{
		"name": "boost_get", "arguments": map[string]any{"id": "22222222-2222-2222-2222-222222222222"},
	}, time.Now())
	result := postMCP(t, s.mcpHandler(), "192.168.107.51", sealed, nil)
	if result.Code != 200 {
		t.Fatal(result.Code)
	}
	plaintext, err := response.OpenResponse(result.Body.Bytes())
	if err != nil || !containsPlain(string(plaintext), "Shared guidance") {
		t.Fatalf("worker on shared gateway: %s (%v)", plaintext, err)
	}
	requireNoSecretLeak(t, s, plain)
	_ = gwPriv
}

func containsPlain(s, sub string) bool { return strings.Contains(s, sub) }
