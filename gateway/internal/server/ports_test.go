package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkerPostPortsCreatesAutoRoutes(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/worker/ports", bytes.NewReader([]byte(
		`{"worker":"piso-worker-demo","slug":"demo","ports":[{"port":8080,"reachable":true},{"port":14970,"reachable":false,"note":"bound to loopback 127.0.0.1"}]}`,
	)))
	req.Header.Set("Content-Type", "application/json")
	wh.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("post ports %d %s", w.Code, w.Body.String())
	}
	r, ok := s.Store.RouteByName("demo-8080")
	if !ok || r.Worker != "piso-worker-demo" || r.Port != 8080 || r.Origin != "auto" {
		t.Fatalf("route %+v ok=%v", r, ok)
	}
	// unreachable hint recorded
	hints, ok := s.Store.WorkerUnreachable("piso-worker-demo")
	if !ok || len(hints) != 1 || hints[0].Port != 14970 {
		t.Fatalf("hints %+v ok=%v", hints, ok)
	}
	// origin + heartbeat serialize (dashboard depends on them)
	body, _ := json.Marshal(s.Store.RouteByName("demo-8080"))
	if !strings.Contains(string(body), `"origin":"auto"`) || !strings.Contains(string(body), `"lastSeenMs"`) {
		t.Fatalf("route view missing fields: %s", body)
	}
}

func TestWorkerPostPortsSlugDefaultsAndDedupes(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/worker/ports", bytes.NewReader([]byte(
		`{"worker":"piso-worker-mixed","ports":[
		   {"port":8080,"reachable":true},{"port":8080,"reachable":true},
		   {"port":99999,"reachable":true},{"port":0,"reachable":true}]}`,
	)))
	req.Header.Set("Content-Type", "application/json")
	wh.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("post %d %s", w.Code, w.Body.String())
	}
	r, ok := s.Store.RouteByName("mixed-8080") // slug derived from worker name
	if !ok || r.Origin != "auto" {
		t.Fatalf("dedupe/slug route %+v ok=%v", r, ok)
	}
	// 99999 (invalid) and 0 were dropped, so no out-of-range route exists
	if _, found := s.Store.RouteByName("mixed-99999"); found {
		t.Fatal("invalid port was routed")
	}
	if _, found := s.Store.RouteByName("mixed-0"); found {
		t.Fatal("zero port was routed")
	}
}

func TestWorkerPostPortsValidation(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()

	if w := doJSON(t, wh, "POST", "/api/v1/worker/ports", map[string]any{
		"worker": "../etc", "ports": []map[string]any{{"port": 8080, "reachable": true}},
	}); w.Code != 400 {
		t.Fatalf("bad worker %d", w.Code)
	}
	w := httptest.NewRecorder()
	wh.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/worker/ports", strings.NewReader("{not json")))
	if w.Code != 400 {
		t.Fatalf("bad json %d %s", w.Code, w.Body.String())
	}
}

func TestWorkerPostPortsIdentityMismatch(t *testing.T) {
	s := testServer(t)
	// register a worker whose vpc IP is the request's source
	if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/workers", map[string]any{
		"name": "piso-worker-demo", "slug": "demo", "ips": []string{"192.168.107.50"},
	}); w.Code != 200 {
		t.Fatalf("register %d %s", w.Code, w.Body.String())
	}
	// same source IP, claiming a different identity → rejected
	raw, _ := json.Marshal(map[string]any{
		"worker": "piso-worker-other", "slug": "other", "ports": []map[string]any{{"port": 8080, "reachable": true}},
	})
	req := httptest.NewRequest("POST", "/api/v1/worker/ports", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.168.107.50:12345"
	w := httptest.NewRecorder()
	s.WorkerHandler().ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("identity spoof accepted: %d %s", w.Code, w.Body.String())
	}
}
