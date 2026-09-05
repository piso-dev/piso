package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)
func TestPostWorkersRegistersAndLists(t *testing.T) {
	s := testServer(t)
	ch := s.ControlHandler()

	// register a worker from the host CLI
	res := doJSON(t, ch, "POST", "/api/v1/workers", map[string]any{
		"name": "piso-worker-demo", "slug": "demo", "ips": []string{"192.168.107.50", "192.168.107.50"},
	})
	if res.Code != 200 {
		t.Fatalf("post workers %d %s", res.Code, res.Body.String())
	}
	var rec struct {
		Name string   `json:"name"`
		Slug string   `json:"slug"`
		IPs  []string `json:"ips"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Name != "piso-worker-demo" || rec.Slug != "demo" {
		t.Fatalf("rec %+v", rec)
	}
	// duplicate IPs deduped
	if len(rec.IPs) != 1 || rec.IPs[0] != "192.168.107.50" {
		t.Fatalf("IPs %+v", rec.IPs)
	}

	// listed
	list := doJSON(t, ch, "GET", "/api/v1/workers", nil)
	if list.Code != 200 || !strings.Contains(list.Body.String(), "demo") {
		t.Fatalf("list %d %s", list.Code, list.Body.String())
	}
}

func TestWorkersRejectsBadIPAndMissingName(t *testing.T) {
	s := testServer(t)
	ch := s.ControlHandler()

	res := doJSON(t, ch, "POST", "/api/v1/workers", map[string]any{
		"name": "piso-worker-demo", "slug": "demo", "ips": []string{"not-an-ip"},
	})
	if res.Code != 400 {
		t.Fatalf("bad ip accepted: %d %s", res.Code, res.Body.String())
	}
	res = doJSON(t, ch, "POST", "/api/v1/workers", map[string]any{
		"slug": "demo", "ips": []string{"192.168.107.50"},
	})
	if res.Code != 400 {
		t.Fatalf("missing name accepted: %d %s", res.Code, res.Body.String())
	}
}

func TestWorkersEndpointNotOnWorkerHandler(t *testing.T) {
	s := testServer(t)
	// the workers registry is host-only; the worker API must not expose it
	wh := s.WorkerHandler()
	w := doJSON(t, wh, "POST", "/api/v1/workers", map[string]any{
		"name": "piso-worker-x", "slug": "x", "ips": []string{"192.168.107.1"},
	})
	if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("worker handler should not serve /api/v1/workers, got %d", w.Code)
	}
}

func TestSetWorkerInternetToggle(t *testing.T) {
	s := testServer(t)
	ch := s.ControlHandler()
	// register first
	if w := doJSON(t, ch, "POST", "/api/v1/workers", map[string]any{
		"name": "piso-worker-demo", "slug": "demo", "ips": []string{"192.168.107.50"},
	}); w.Code != 200 {
		t.Fatalf("register %d %s", w.Code, w.Body.String())
	}
	// toggle off
	toggled := doJSON(t, ch, "POST", "/api/v1/workers/piso-worker-demo/internet", map[string]any{"disabled": true})
	if toggled.Code != 200 || !strings.Contains(toggled.Body.String(), "\"internetDisabled\":true") {
		t.Fatalf("toggle on %d %s", toggled.Code, toggled.Body.String())
	}
	// verify via GET
	list := doJSON(t, ch, "GET", "/api/v1/workers", nil)
	if !strings.Contains(list.Body.String(), "\"internetDisabled\":true") {
		t.Fatalf("registry missing flag: %s", list.Body.String())
	}
	// unknown worker → 404
	if w := doJSON(t, ch, "POST", "/api/v1/workers/piso-worker-nope/internet", map[string]any{"disabled": true}); w.Code != 404 {
		t.Fatalf("unknown worker should 404, got %d", w.Code)
	}
	// bad name → 400
	if w := doJSON(t, ch, "POST", "/api/v1/workers/not-a-valid-name!!/internet", map[string]any{"disabled": true}); w.Code != 400 {
		t.Fatalf("bad name should 400, got %d", w.Code)
	}
}