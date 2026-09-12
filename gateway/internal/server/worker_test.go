package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postCheckin posts an identity claim from a chosen source IP and returns
// the recorder. RemoteAddr defaults to the recorder's loopback when ip is "".
func postCheckin(t *testing.T, h http.Handler, ip, worker, slug string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if slug == "" {
		raw, err := json.Marshal(map[string]any{"worker": worker})
		if err != nil {
			t.Fatal(err)
		}
		req = httptest.NewRequest("POST", "/api/v1/worker/checkin", bytes.NewReader(raw))
	} else {
		raw, err := json.Marshal(map[string]any{"worker": worker, "slug": slug})
		if err != nil {
			t.Fatal(err)
		}
		req = httptest.NewRequest("POST", "/api/v1/worker/checkin", bytes.NewReader(raw))
	}
	req.Header.Set("Content-Type", "application/json")
	if ip != "" {
		req.RemoteAddr = ip + ":12345"
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// An unclaimed IP is claimed by the first worker that checks in (the
// self-heal path: a recreated container's new vpc IP replaces nothing).
func TestWorkerCheckinClaimsUnclaimedIP(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	w := postCheckin(t, wh, "192.168.107.50", "piso-worker-demo", "demo")
	if w.Code != 200 {
		t.Fatalf("checkin %d %s", w.Code, w.Body.String())
	}
	rec, ok := s.Store.WorkerByIP("192.168.107.50")
	if !ok || rec.Name != "piso-worker-demo" || rec.Slug != "demo" {
		t.Fatalf("registry %+v ok=%v", rec, ok)
	}
}

// A second checkin from the same IP is a no-op (idempotent, single IP entry).
func TestWorkerCheckinIdempotent(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	if w := postCheckin(t, wh, "192.168.107.50", "piso-worker-demo", "demo"); w.Code != 200 {
		t.Fatalf("first %d %s", w.Code, w.Body.String())
	}
	if w := postCheckin(t, wh, "192.168.107.50", "piso-worker-demo", "demo"); w.Code != 200 {
		t.Fatalf("second %d %s", w.Code, w.Body.String())
	}
	rec, _ := s.Store.WorkerByName("piso-worker-demo")
	if len(rec.IPs) != 1 || rec.IPs[0] != "192.168.107.50" {
		t.Fatalf("expected single IP, got %+v", rec.IPs)
	}
}

// Checking in from a NEW IP appends to the registered set instead of
// clobbering it (a worker may have several vpc addresses).
func TestWorkerCheckinAppendsExistingIPs(t *testing.T) {
	s := testServer(t)
	if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/workers", map[string]any{
		"name": "piso-worker-demo", "slug": "demo", "ips": []string{"10.0.0.7"},
	}); w.Code != 200 {
		t.Fatalf("register %d %s", w.Code, w.Body.String())
	}
	wh := s.WorkerHandler()
	if w := postCheckin(t, wh, "10.0.0.8", "piso-worker-demo", "demo"); w.Code != 200 {
		t.Fatalf("checkin %d %s", w.Code, w.Body.String())
	}
	rec, _ := s.Store.WorkerByName("piso-worker-demo")
	if len(rec.IPs) != 2 || !strings.Contains(strings.Join(rec.IPs, ","), "10.0.0.7") ||
		!strings.Contains(strings.Join(rec.IPs, ","), "10.0.0.8") {
		t.Fatalf("expected merged IPs, got %+v", rec.IPs)
	}
}

// An IP already owned by ANOTHER worker cannot be re-claimed — strict
// anti-squatting; the checkin response carries a diagnostic detail.
func TestWorkerCheckinCrossNameRejected(t *testing.T) {
	s := testServer(t)
	if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/workers", map[string]any{
		"name": "piso-worker-demo", "slug": "demo", "ips": []string{"192.168.107.50"},
	}); w.Code != 200 {
		t.Fatalf("register %d %s", w.Code, w.Body.String())
	}
	wh := s.WorkerHandler()
	w := postCheckin(t, wh, "192.168.107.50", "piso-worker-other", "other")
	if w.Code != 403 {
		t.Fatalf("cross-name claim accepted: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "identity mismatch") || !strings.Contains(body, "request claims") {
		t.Fatalf("403 missing diagnostic detail: %s", body)
	}
	// the conflicting claim must not have touched the registry
	if _, ok := s.Store.WorkerByIP("192.168.107.50"); !ok {
		t.Fatal("registry lost the original owner")
	}
	if reg, ok := s.Store.WorkerByName("piso-worker-other"); ok && len(reg.IPs) > 0 {
		t.Fatalf("claim leaked an IP: %+v", reg)
	}
}

// Same worker, same IP, different slug is also a real conflict → 403.
func TestWorkerCheckinSlugMismatchRejected(t *testing.T) {
	s := testServer(t)
	if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/workers", map[string]any{
		"name": "piso-worker-demo", "slug": "demo", "ips": []string{"192.168.107.50"},
	}); w.Code != 200 {
		t.Fatalf("register %d %s", w.Code, w.Body.String())
	}
	wh := s.WorkerHandler()
	if w := postCheckin(t, wh, "192.168.107.50", "piso-worker-demo", "other"); w.Code != 403 {
		t.Fatalf("slug spoof accepted: %d %s", w.Code, w.Body.String())
	}
}

// getFrom issues a GET from a chosen source IP (mirrors postCheckin).
func getFrom(t *testing.T, h http.Handler, ip, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if ip != "" {
		req.RemoteAddr = ip + ":12345"
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// The gateway reports LIVE boundaries: the internet kill-switch (already in
// the registry) and which placeholders this worker's model actually sees.
func TestWorkerCapabilitiesBasics(t *testing.T) {
	s := testServer(t)
	if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/workers", map[string]any{
		"name": "piso-worker-demo", "slug": "demo", "ips": []string{"192.168.107.50"},
	}); w.Code != 200 {
		t.Fatalf("register %d %s", w.Code, w.Body.String())
	}
	// one secret scoped to demo, another to a different slug
	if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/secrets", map[string]any{
		"name": "demo-key", "placeholder": "piso_demo_key", "value": "sk-test", "envKey": "DEMO_KEY",
		"workers": []string{"demo"},
	}); w.Code != 201 {
		t.Fatalf("add secret %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/secrets", map[string]any{
		"name": "other-key", "placeholder": "piso_other_key", "value": "sk-test2", "envKey": "OTHER_KEY",
		"workers": []string{"other"},
	}); w.Code != 201 {
		t.Fatalf("add secret %d %s", w.Code, w.Body.String())
	}
	wh := s.WorkerHandler()
	w := getFrom(t, wh, "192.168.107.50", "/api/v1/worker/capabilities?worker=piso-worker-demo")
	if w.Code != 200 {
		t.Fatalf("capabilities %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"internetEnabled":true`) || !strings.Contains(body, "piso_demo_key") {
		t.Fatalf("capabilities body %s", body)
	}
	if strings.Contains(body, "piso_other_key") {
		t.Fatalf("leaked unrelated secret scope: %s", body)
	}
	// kill the internet switch → the flag flips for the same worker
	if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/workers/piso-worker-demo/internet", map[string]any{
		"disabled": true,
	}); w.Code != 200 {
		t.Fatalf("toggle %d %s", w.Code, w.Body.String())
	}
	w = getFrom(t, wh, "192.168.107.50", "/api/v1/worker/capabilities?worker=piso-worker-demo")
	if !strings.Contains(w.Body.String(), `"internetEnabled":false`) {
		t.Fatalf("internet flag not flipped: %s", w.Body.String())
	}
}

// Capability reads are identity-checked like every other worker-API call.
func TestWorkerCapabilitiesIdentity(t *testing.T) {
	s := testServer(t)
	if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/workers", map[string]any{
		"name": "piso-worker-demo", "slug": "demo", "ips": []string{"192.168.107.50"},
	}); w.Code != 200 {
		t.Fatalf("register %d %s", w.Code, w.Body.String())
	}
	w := getFrom(t, s.WorkerHandler(), "192.168.107.50", "/api/v1/worker/capabilities?worker=piso-worker-other")
	if w.Code != 403 {
		t.Fatalf("capabilities identity %d %s", w.Code, w.Body.String())
	}
}

// The `host` kind is a valid activity kind (Host Action Request protocol).
func TestWorkerActivityAcceptsHostKind(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	if w := doJSON(t, wh, "POST", "/api/v1/worker/activity", map[string]any{
		"worker": "piso-worker-demo", "kind": "host",
		"text": "Clone git@github.com:org/repo.git -> /workspace/repo",
	}); w.Code != 201 {
		t.Fatalf("host kind %d %s", w.Code, w.Body.String())
	}
}

func TestWorkerCheckinValidation(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	rec := httptest.NewRecorder()
	wh.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/worker/checkin", strings.NewReader("{not json")))
	if rec.Code != 400 {
		t.Fatalf("bad json %d", rec.Code)
	}
	if w := postCheckin(t, wh, "", "../etc", "demo"); w.Code != 400 {
		t.Fatalf("bad worker %d", w.Code)
	}
	if w := postCheckin(t, wh, "", "piso-worker-demo", "../oops"); w.Code != 400 {
		t.Fatalf("bad slug %d", w.Code)
	}
	// name passes, but a slug *derived* from it is empty → 400
	if w := postCheckin(t, wh, "", "piso-worker-", ""); w.Code != 400 {
		t.Fatalf("bad derived slug %d", w.Code)
	}
}