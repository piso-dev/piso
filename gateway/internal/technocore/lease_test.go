package technocore

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTelegramLeaseSendsResourceAndCursor(t *testing.T) {
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
			`{"granted":true,"holder":"gw","ttl":120,"expires_at":"2026-01-02T03:04:05+00:00","offset":77}`))
	}))
	defer srv.Close()

	_, dataDir, _ := enrolledShareEnv(t, srv.URL)
	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	grant, err := TelegramLease(dataDir, "11111111-1111-1111-1111-111111111111", 42, 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/v1/telegram/lease" || got.method != http.MethodPost {
		t.Fatalf("request %s %s", got.method, got.path)
	}
	if got.body["resource_id"] != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("resource_id %v", got.body["resource_id"])
	}
	if got.body["offset"] != float64(42) || got.body["ttl"] != float64(120) {
		t.Fatalf("body %v", got.body)
	}
	if !grant.Granted || grant.Offset != 77 || grant.Holder != "gw" {
		t.Fatalf("grant %+v", grant)
	}
	if grant.ExpiresAt.IsZero() {
		t.Fatal("expires_at did not parse")
	}
}

func TestTelegramLeaseRefusalIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"granted":false,"holder":"other","offset":5}`))
	}))
	defer srv.Close()

	_, dataDir, _ := enrolledShareEnv(t, srv.URL)
	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	grant, err := TelegramLease(dataDir, "rid", 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Granted || grant.Holder != "other" || grant.Offset != 5 {
		t.Fatalf("grant %+v", grant)
	}
}

func TestTelegramLeaseRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer srv.Close()

	_, dataDir, _ := enrolledShareEnv(t, srv.URL)
	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()

	if _, err := TelegramLease(dataDir, "rid", 0, time.Second); err == nil {
		t.Fatal("expected an error for a non-200 lease response")
	}
}
