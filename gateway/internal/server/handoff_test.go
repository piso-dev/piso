package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

func TestCheckinAdvertisesPublicURL(t *testing.T) {
	s := testServer(t)
	dir := t.TempDir()
	s.DataDir = dir
	id, err := tc.Generate("https://coord.example", "gw")
	if err != nil {
		t.Fatal(err)
	}
	id.Status = tc.StatusActive
	id.Placement = tc.PlacementStandalone
	id.PublicURL = "https://gw.example:8443"
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}

	tok := "worker-bootstrap-token"
	sum := sha256.Sum256([]byte(tok))
	if err := s.Store.ReplaceRemoteWorkers([]store.WorkerRec{{
		Name: "piso-worker-demo", Slug: "demo", TokenHash: hex.EncodeToString(sum[:]), Remote: true,
	}}); err != nil {
		t.Fatal(err)
	}

	// Relay path: WorkerHandler directly, no via header. Must not count as switched.
	body := `{"worker":"piso-worker-demo","slug":"demo"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/checkin", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.WorkerHandler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("relay checkin %d %s", w.Code, w.Body.String())
	}
	var out struct {
		PublicURL string `json:"publicUrl"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.PublicURL != "https://gw.example:8443" {
		t.Fatalf("publicUrl %q", out.PublicURL)
	}
	if !s.RelayStillNeeded() {
		t.Fatal("relay check-in must not clear the pool")
	}

	// Direct path: public mux sets the via header.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/worker/checkin", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.PublicWorkerHandler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("direct checkin %d %s", w.Code, w.Body.String())
	}
	if s.RelayStillNeeded() {
		t.Fatal("direct check-in of the only remote worker should release the pool")
	}
}

func TestRelayStaysUntilEveryRemoteWorkerSwitches(t *testing.T) {
	s := testServer(t)
	dir := t.TempDir()
	s.DataDir = dir
	id, err := tc.Generate("https://coord.example", "gw")
	if err != nil {
		t.Fatal(err)
	}
	id.Status = tc.StatusActive
	id.PublicURL = "https://gw.example:8443"
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}
	sumA := sha256.Sum256([]byte("tok-a"))
	sumB := sha256.Sum256([]byte("tok-b"))
	if err := s.Store.ReplaceRemoteWorkers([]store.WorkerRec{
		{Name: "piso-worker-a", Slug: "a", TokenHash: hex.EncodeToString(sumA[:]), Remote: true},
		{Name: "piso-worker-b", Slug: "b", TokenHash: hex.EncodeToString(sumB[:]), Remote: true},
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/worker/checkin", strings.NewReader(`{"worker":"piso-worker-a","slug":"a"}`))
	req.Header.Set("Authorization", "Bearer tok-a")
	w := httptest.NewRecorder()
	s.PublicWorkerHandler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("a %d %s", w.Code, w.Body.String())
	}
	if !s.RelayStillNeeded() {
		t.Fatal("one of two workers switched; pool must stay")
	}
}
