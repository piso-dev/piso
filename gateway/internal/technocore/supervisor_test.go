package technocore

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSupervisorLeaseSendsWorkerAndReadsGrant(t *testing.T) {
	var got struct {
		path   string
		method string
		body   map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.method = r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"granted":true,"holder":"w1","holder_principal":"p1","ttl":120,"expires_at":"2026-01-02T03:04:05+00:00"}`))
	}))
	defer srv.Close()

	_, dataDir, _ := enrolledShareEnv(t, srv.URL)
	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	grant, err := SupervisorLease(dataDir, "11111111-1111-1111-1111-111111111111", "p1", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/v1/telegram/bots/11111111-1111-1111-1111-111111111111/supervisor-lease" || got.method != http.MethodPost {
		t.Fatalf("request %s %s", got.method, got.path)
	}
	if got.body["worker_id"] != "p1" || got.body["ttl"] != float64(120) {
		t.Fatalf("body %v", got.body)
	}
	if !grant.Granted || grant.Holder != "w1" || grant.HolderPrincipal != "p1" || grant.ExpiresAt.IsZero() {
		t.Fatalf("grant %+v", grant)
	}
}

// A refusal is an answer, not a fault: the caller must be able to read the
// reason and keep asking rather than back off as if the call failed.
func TestSupervisorLeaseRefusalIsAnAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"granted":false,"reason":"not_standalone"}`))
	}))
	defer srv.Close()

	_, dataDir, _ := enrolledShareEnv(t, srv.URL)
	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	grant, err := SupervisorLease(dataDir, "rid", "p1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Granted || grant.Reason != "not_standalone" {
		t.Fatalf("grant %+v", grant)
	}
}

func TestSupervisorLeaseRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer srv.Close()

	_, dataDir, _ := enrolledShareEnv(t, srv.URL)
	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	if _, err := SupervisorLease(dataDir, "rid", "p1", time.Second); err == nil {
		t.Fatal("expected an error for a non-200 supervisor response")
	}
}
