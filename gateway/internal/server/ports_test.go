package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"piso/gateway/internal/store"
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
	rec, _ := s.Store.RouteByName("demo-8080")
	body, _ := json.Marshal(rec)
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

// A disabled route refuses to proxy (403) but stays listed for the toggle;
// re-enabling serves again.
func TestRouteDisabledBlocksProxy(t *testing.T) {
	s := testServer(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("from-worker"))
	}))
	t.Cleanup(backend.Close)
	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AddRoute(store.RouteRec{ID: "r1", Name: "demo-8080", Worker: "127.0.0.1", Port: port, Origin: store.AutoRouteOriginAuto, LastSeenMs: 1}); err != nil {
		t.Fatal(err)
	}
	h := s.WebHandler()
	hit := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://demo-8080.piso.local/", nil)
		req.Host = "demo-8080.piso.local"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := hit(); w.Code != 200 || !strings.Contains(w.Body.String(), "from-worker") {
		t.Fatalf("pre-disable %d %s", w.Code, w.Body.String())
	}
	// disable via the control API
	raw, _ := json.Marshal(map[string]any{"disabled": true})
	req := httptest.NewRequest("POST", "/api/v1/routes/r1/disabled", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	wc := httptest.NewRecorder()
	s.ControlHandler().ServeHTTP(wc, req)
	if wc.Code != 200 {
		t.Fatalf("disable api %d %s", wc.Code, wc.Body.String())
	}
	if w := hit(); w.Code != http.StatusForbidden {
		t.Fatalf("disabled route should 403, got %d %s", w.Code, w.Body.String())
	}
	// re-enable
	raw, _ = json.Marshal(map[string]any{"disabled": false})
	req = httptest.NewRequest("POST", "/api/v1/routes/r1/disabled", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	wc = httptest.NewRecorder()
	s.ControlHandler().ServeHTTP(wc, req)
	if wc.Code != 200 {
		t.Fatalf("enable api %d %s", wc.Code, wc.Body.String())
	}
	if w := hit(); w.Code != 200 {
		t.Fatalf("re-enabled should serve, got %d", w.Code)
	}
}

func TestWorkerActivityPostGetRevoke(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	// post an activity
	res := doJSON(t, wh, "POST", "/api/v1/worker/activity", map[string]any{
		"worker": "piso-worker-demo", "kind": "progress", "text": "Started OAuth flow",
	})
	if res.Code != 201 {
		t.Fatalf("post %d %s", res.Code, res.Body.String())
	}
	var act store.Activity
	_ = json.Unmarshal(res.Body.Bytes(), &act)
	if act.ID == "" || act.Kind != "progress" || act.Slug != "demo" || act.Text != "Started OAuth flow" {
		t.Fatalf("act %+v", act)
	}
	// control plane read (host board)
	ctrl := doJSON(t, s.ControlHandler(), "GET", "/api/v1/activities", nil)
	if ctrl.Code != 200 || !strings.Contains(ctrl.Body.String(), act.ID) {
		t.Fatalf("control read %d %s", ctrl.Code, ctrl.Body.String())
	}
	// worker feed read
	feed := doJSON(t, wh, "GET", "/api/v1/worker/activities?worker=piso-worker-demo", nil)
	if feed.Code != 200 || !strings.Contains(feed.Body.String(), "Started OAuth flow") {
		t.Fatalf("feed %d %s", feed.Code, feed.Body.String())
	}
	// revoke by a different worker → 404 (scoped)
	rev := doJSON(t, wh, "POST", "/api/v1/worker/activity/revoke", map[string]any{"id": act.ID, "worker": "piso-worker-other"})
	if rev.Code != 404 {
		t.Fatalf("cross-worker revoke %d %s", rev.Code, rev.Body.String())
	}
	// revoke by owner → 204
	rev = doJSON(t, wh, "POST", "/api/v1/worker/activity/revoke", map[string]any{"id": act.ID, "worker": "piso-worker-demo"})
	if rev.Code != 204 {
		t.Fatalf("own revoke %d %s", rev.Code, rev.Body.String())
	}
}

func TestWorkerActivityValidation(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	// bad worker
	if w := doJSON(t, wh, "POST", "/api/v1/worker/activity", map[string]any{"worker": "../etc", "kind": "note", "text": "x"}); w.Code != 400 {
		t.Fatalf("bad worker %d", w.Code)
	}
	// unknown kind
	if w := doJSON(t, wh, "POST", "/api/v1/worker/activity", map[string]any{"worker": "piso-worker-demo", "kind": "bogus", "text": "x"}); w.Code != 400 {
		t.Fatalf("bad kind %d %s", w.Code, w.Body.String())
	}
	// empty text
	if w := doJSON(t, wh, "POST", "/api/v1/worker/activity", map[string]any{"worker": "piso-worker-demo", "kind": "note", "text": "  "}); w.Code != 400 {
		t.Fatalf("empty text %d", w.Code)
	}
	// control-plane read without worker (board) still works; worker feed needs a worker
	if w := doJSON(t, wh, "GET", "/api/v1/worker/activities", nil); w.Code != 400 {
		t.Fatalf("feed without worker %d", w.Code)
	}
}
