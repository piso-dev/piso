package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"piso/gateway/internal/store"
)

func TestPublicWorkerRequiresToken(t *testing.T) {
	s := testServer(t)
	h := s.PublicWorkerHandler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/worker/health", nil))
	if w.Code != 401 {
		t.Fatalf("unauthenticated GET %d %s", w.Code, w.Body.String())
	}

	tok := "worker-bootstrap-token"
	sum := sha256.Sum256([]byte(tok))
	if err := s.Store.ReplaceRemoteWorkers([]store.WorkerRec{{
		Name: "piso-worker-demo", Slug: "demo", TokenHash: hex.EncodeToString(sum[:]),
	}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/worker/health", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("authenticated GET %d %s", w.Code, w.Body.String())
	}
}

func TestPublicConnectRequiresProxyAuth(t *testing.T) {
	s := testServer(t)
	h := s.PublicWorkerHandler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodConnect, "https://example.com:443", nil))
	if w.Code != http.StatusProxyAuthRequired {
		t.Fatalf("CONNECT without auth %d", w.Code)
	}
}
