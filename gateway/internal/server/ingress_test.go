package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), 20)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Store: st}
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPlanningRouteNameAndSlug(t *testing.T) {
	if got := planningRouteName("My_Project"); got != "plan-my-project" {
		t.Fatalf("got %q", got)
	}
	if got := slugFromWorker("piso-worker-demo"); got != "demo" {
		t.Fatalf("got %q", got)
	}
}

func TestCreateIngressPendingThenApprove(t *testing.T) {
	s := testServer(t)
	h := s.ControlHandler()
	created := doJSON(t, h, "POST", "/api/v1/ingress/requests", map[string]any{
		"kind": "planning", "worker": "piso-worker-demo", "port": 19432,
	})
	if created.Code != 201 {
		t.Fatalf("create %d %s", created.Code, created.Body.String())
	}
	var rec ingressView
	if err := json.Unmarshal(created.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Name != "plan-demo" || rec.Status != store.IngressStatusPending || rec.URL == "" || !strings.Contains(rec.URL, "piso.local") || !strings.Contains(rec.URL, "route=plan-demo") {
		t.Fatalf("view %+v", rec)
	}
	listed := doJSON(t, h, "GET", "/api/v1/ingress/requests", nil)
	if listed.Code != 200 || !strings.Contains(listed.Body.String(), rec.ID) {
		t.Fatalf("list %d %s", listed.Code, listed.Body.String())
	}
	again := doJSON(t, h, "POST", "/api/v1/ingress/requests", map[string]any{
		"worker": "piso-worker-demo", "port": 19432,
	})
	if again.Code != 200 {
		t.Fatalf("upsert %d %s", again.Code, again.Body.String())
	}
	approved := doJSON(t, h, "POST", "/api/v1/ingress/requests/"+rec.ID+"/approve", nil)
	if approved.Code != 200 {
		t.Fatalf("approve %d %s", approved.Code, approved.Body.String())
	}
	var out struct {
		URL   string         `json:"url"`
		Route store.RouteRec `json:"route"`
	}
	if err := json.Unmarshal(approved.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Route.Name != "plan-demo" || out.URL == "" {
		t.Fatalf("approve %+v", out)
	}
	empty := doJSON(t, h, "GET", "/api/v1/ingress/requests", nil)
	if empty.Body.String() != "[]\n" && !strings.Contains(empty.Body.String(), "[]") {
		t.Fatalf("pending after approve: %s", empty.Body.String())
	}
	live := doJSON(t, h, "POST", "/api/v1/ingress/requests", map[string]any{
		"worker": "piso-worker-demo", "port": 19432,
	})
	if live.Code != 200 {
		t.Fatalf("live %d %s", live.Code, live.Body.String())
	}
	var liveView ingressView
	if err := json.Unmarshal(live.Body.Bytes(), &liveView); err != nil {
		t.Fatal(err)
	}
	if liveView.Status != "approved" || liveView.ID != "" {
		t.Fatalf("expected approved no-pending, got %+v", liveView)
	}
}

func TestCreateIngressRejectsBadWorkerAndUnknownKind(t *testing.T) {
	s := testServer(t)
	h := s.ControlHandler()
	bad := doJSON(t, h, "POST", "/api/v1/ingress/requests", map[string]any{"worker": "../etc"})
	if bad.Code != 400 {
		t.Fatalf("bad worker %d", bad.Code)
	}
	kind := doJSON(t, h, "POST", "/api/v1/ingress/requests", map[string]any{
		"worker": "piso-worker-demo", "kind": "preview",
	})
	if kind.Code != 400 {
		t.Fatalf("kind %d %s", kind.Code, kind.Body.String())
	}
}

func TestApproveIngressConflictAndDismiss(t *testing.T) {
	s := testServer(t)
	h := s.ControlHandler()
	if err := s.Store.AddRoute(store.RouteRec{ID: "route_x", Name: "plan-demo", Worker: "piso-worker-other", Port: 80}); err != nil {
		t.Fatal(err)
	}
	created := doJSON(t, h, "POST", "/api/v1/ingress/requests", map[string]any{
		"worker": "piso-worker-demo",
	})
	if created.Code != 201 {
		t.Fatalf("create %d %s", created.Code, created.Body.String())
	}
	var rec ingressView
	if err := json.Unmarshal(created.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	conflict := doJSON(t, h, "POST", "/api/v1/ingress/requests/"+rec.ID+"/approve", nil)
	if conflict.Code != 409 {
		t.Fatalf("conflict %d %s", conflict.Code, conflict.Body.String())
	}
	dismiss := doJSON(t, h, "POST", "/api/v1/ingress/requests/"+rec.ID+"/dismiss", nil)
	if dismiss.Code != 204 {
		t.Fatalf("dismiss %d %s", dismiss.Code, dismiss.Body.String())
	}
	listed := doJSON(t, h, "GET", "/api/v1/ingress/requests", nil)
	if strings.Contains(listed.Body.String(), rec.ID) {
		t.Fatalf("still listed: %s", listed.Body.String())
	}
}

func TestCancelIngressByWorker(t *testing.T) {
	s := testServer(t)
	h := s.ControlHandler()
	if doJSON(t, h, "POST", "/api/v1/ingress/requests", map[string]any{"worker": "piso-worker-demo"}).Code != 201 {
		t.Fatal("create")
	}
	cancel := doJSON(t, h, "POST", "/api/v1/ingress/requests/cancel", map[string]any{"worker": "piso-worker-demo"})
	if cancel.Code != 200 || !strings.Contains(cancel.Body.String(), `"cancelled":1`) {
		t.Fatalf("cancel %d %s", cancel.Code, cancel.Body.String())
	}
}

func TestIngressApexQuerySetsCookieAndPrettyHost(t *testing.T) {
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
	if err := s.Store.AddRoute(store.RouteRec{ID: "r1", Name: "plan-demo", Worker: "127.0.0.1", Port: port}); err != nil {
		t.Fatal(err)
	}
	h := s.IngressHandler()

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://piso.local:8082/?route=plan-demo", nil)
	req.Host = "piso.local:8082"
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("redirect %d", w.Code)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == ingressRouteCookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value != "plan-demo" {
		t.Fatalf("cookie %v", w.Result().Cookies())
	}

	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "http://piso.local:8082/", nil)
	req2.Host = "piso.local:8082"
	req2.AddCookie(cookie)
	h.ServeHTTP(w2, req2)
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), "from-worker") {
		t.Fatalf("cookie route %d %s", w2.Code, w2.Body.String())
	}

	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "http://plan-demo.piso.local/", nil)
	req3.Host = "plan-demo.piso.local"
	h.ServeHTTP(w3, req3)
	if w3.Code != 200 || !strings.Contains(w3.Body.String(), "from-worker") {
		t.Fatalf("pretty host %d %s", w3.Code, w3.Body.String())
	}
}

func TestIngressPublicURLUsesEnvPort(t *testing.T) {
	t.Setenv("PISO_INGRESS_PORT", "8443")
	if got := ingressPublicURL("plan-demo"); got != "http://piso.local:8443/?route=plan-demo" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("PISO_INGRESS_PORT", "80")
	if got := ingressPublicURL("plan-demo"); got != "http://piso.local/?route=plan-demo" {
		t.Fatalf("got %q", got)
	}
}
