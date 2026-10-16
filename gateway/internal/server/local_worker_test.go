package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

func TestHostRegistrationEnrollsLocalWorker(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	calls := 0
	const principal = "22222222-2222-2222-2222-222222222222"
	coord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/api/v1/local-workers" || r.Header.Get("X-Piso-Signature") == "" {
			t.Errorf("unsigned or unexpected request: %s %s", r.Method, r.URL)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if len(body) != 1 || body["slug"] != "demo" {
			t.Errorf("body: %v", body)
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"principal_id": principal, "slug": "demo"})
	}))
	defer coord.Close()
	id, err := tc.Generate(coord.URL, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	id.Status = tc.StatusActive
	if err := tc.Save(tc.Path(s.DataDir), id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/workers", map[string]any{
			"name": "piso-worker-demo", "slug": "demo", "ips": []string{"192.168.107.50"},
		})
		var rec store.WorkerRec
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rec) != nil || rec.PrincipalID != principal || rec.Remote {
			t.Fatalf("registration: %d %s", w.Code, w.Body.String())
		}
		if i == 0 {
			s.Store.BindChannelKey(rec.Name, "bound-key")
		}
	}
	rec, ok := s.Store.WorkerByIP("192.168.107.50")
	if !ok || rec.ChannelPubkey != "bound-key" || rec.PrincipalID != principal || calls != 1 {
		t.Fatalf("identity lost or re-enrolled: %+v calls=%d", rec, calls)
	}
}

func TestHostRegistrationSurvivesCoordinatorFailure(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	coord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer coord.Close()
	id, _ := tc.Generate(coord.URL, "gateway")
	id.Status = tc.StatusActive
	if err := tc.Save(tc.Path(s.DataDir), id); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/workers", map[string]any{
		"name": "piso-worker-demo", "slug": "demo", "ips": []string{"192.168.107.50"},
	})
	rec, ok := s.Store.WorkerByIP("192.168.107.50")
	if w.Code != 200 || !ok || rec.PrincipalID != "" || rec.Remote {
		t.Fatalf("local startup failed: %d %+v", w.Code, rec)
	}
}
