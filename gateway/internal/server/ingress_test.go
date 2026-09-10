package server

import (
	"bytes"
	"encoding/json"
	"net"
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
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
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
	wh := s.WorkerHandler()
	ch := s.ControlHandler()
	created := doJSON(t, wh, "POST", "/api/v1/worker/planning", map[string]any{
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
	listed := doJSON(t, ch, "GET", "/api/v1/ingress/requests", nil)
	if listed.Code != 200 || !strings.Contains(listed.Body.String(), rec.ID) {
		t.Fatalf("list %d %s", listed.Code, listed.Body.String())
	}
	again := doJSON(t, wh, "POST", "/api/v1/worker/planning", map[string]any{
		"worker": "piso-worker-demo", "port": 19432,
	})
	if again.Code != 200 {
		t.Fatalf("upsert %d %s", again.Code, again.Body.String())
	}
	approved := doJSON(t, ch, "POST", "/api/v1/ingress/requests/"+rec.ID+"/approve", nil)
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
	empty := doJSON(t, ch, "GET", "/api/v1/ingress/requests", nil)
	if empty.Body.String() != "[]\n" && !strings.Contains(empty.Body.String(), "[]") {
		t.Fatalf("pending after approve: %s", empty.Body.String())
	}
	againAfter := doJSON(t, wh, "POST", "/api/v1/worker/planning", map[string]any{
		"worker": "piso-worker-demo", "port": 19432,
	})
	if againAfter.Code != 201 {
		t.Fatalf("resubmit after approve %d %s", againAfter.Code, againAfter.Body.String())
	}
	var againView ingressView
	if err := json.Unmarshal(againAfter.Body.Bytes(), &againView); err != nil {
		t.Fatal(err)
	}
	if againView.Status != store.IngressStatusPending || againView.ID == "" {
		t.Fatalf("resubmit must create pending, got %+v", againView)
	}
	listedAgain := doJSON(t, ch, "GET", "/api/v1/ingress/requests", nil)
	if !strings.Contains(listedAgain.Body.String(), againView.ID) {
		t.Fatalf("inbox missing resubmit: %s", listedAgain.Body.String())
	}
}

func TestCreateIngressRejectsBadWorkerAndUnknownKind(t *testing.T) {
	s := testServer(t)
	h := s.WorkerHandler()
	bad := doJSON(t, h, "POST", "/api/v1/worker/planning", map[string]any{"worker": "../etc"})
	if bad.Code != 400 {
		t.Fatalf("bad worker %d", bad.Code)
	}
	kind := doJSON(t, h, "POST", "/api/v1/worker/planning", map[string]any{
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
	created := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/planning", map[string]any{
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
	h := s.WorkerHandler()
	if doJSON(t, h, "POST", "/api/v1/worker/planning", map[string]any{"worker": "piso-worker-demo"}).Code != 201 {
		t.Fatal("create")
	}
	cancel := doJSON(t, h, "POST", "/api/v1/worker/planning/cancel", map[string]any{"worker": "piso-worker-demo"})
	if cancel.Code != 200 || !strings.Contains(cancel.Body.String(), `"cancelled":1`) {
		t.Fatalf("cancel %d %s", cancel.Code, cancel.Body.String())
	}
}

func TestSecretsDeriveEnvKeyAndEdit(t *testing.T) {
	s := testServer(t)
	ch := s.ControlHandler()
	// blank envKey → derived from name
	created := doJSON(t, ch, "POST", "/api/v1/secrets", map[string]any{
		"name": "CLD2 SSH", "placeholder": "piso_cld2_ssh_1", "value": "super-secret",
	})
	if created.Code != 201 {
		t.Fatalf("create %d %s", created.Code, created.Body.String())
	}
	if !strings.Contains(created.Body.String(), `"envKey":"CLD2_SSH"`) {
		t.Fatalf("envKey not derived: %s", created.Body.String())
	}
	recs := s.Store.Secrets()
	if len(recs) != 1 || recs[0].EnvKey != "CLD2_SSH" {
		t.Fatalf("store envKey: %+v", recs)
	}
	id := recs[0].ID
	// placeholder is required
	if doJSON(t, ch, "POST", "/api/v1/secrets", map[string]any{"name": "x", "value": "v"}).Code != 400 {
		t.Fatal("placeholder required")
	}
	// a name that derives nothing must 400 rather than store an empty envKey
	if doJSON(t, ch, "POST", "/api/v1/secrets", map[string]any{"name": "***", "placeholder": "piso_x", "value": "v"}).Code != 400 {
		t.Fatal("all-symbols name should 400")
	}
	// edit: rename + new value + explicit envKey; placeholder carries over
	edited := doJSON(t, ch, "PUT", "/api/v1/secrets/"+id, map[string]any{
		"name": "CLD2", "envKey": "MY_SSH", "value": "sk-2", "allowedHosts": []string{"host.example"},
	})
	if edited.Code != 200 {
		t.Fatalf("edit %d %s", edited.Code, edited.Body.String())
	}
	if !strings.Contains(edited.Body.String(), `"name":"CLD2"`) || !strings.Contains(edited.Body.String(), `"envKey":"MY_SSH"`) {
		t.Fatalf("edit body: %s", edited.Body.String())
	}
	rec := s.Store.Secrets()[0]
	if rec.EnvKey != "MY_SSH" || rec.Placeholder != "piso_cld2_ssh_1" || rec.Value != "sk-2" {
		t.Fatalf("store after edit: %+v", rec)
	}
	// blank value keeps the existing real value
	doJSON(t, ch, "PUT", "/api/v1/secrets/"+id, map[string]any{"name": "CLD2", "envKey": "MY_SSH"})
	rec = s.Store.Secrets()[0]
	if rec.Value != "sk-2" {
		t.Fatalf("blank value should keep existing: %+v", rec)
	}
	// worker scoping is creation-time: an edit must not wipe it
	scoped := doJSON(t, ch, "POST", "/api/v1/secrets", map[string]any{
		"name": "mon", "placeholder": "piso_mon_1", "value": "v", "workers": []string{"monitor"},
	})
	if scoped.Code != 201 {
		t.Fatalf("scoped create %d %s", scoped.Code, scoped.Body.String())
	}
	mid := s.Store.Secrets()[1].ID
	doJSON(t, ch, "PUT", "/api/v1/secrets/"+mid, map[string]any{"name": "mon", "envKey": "MON_KEY"})
	rec = s.Store.Secrets()[1]
	if len(rec.Workers) != 1 || rec.Workers[0] != "monitor" {
		t.Fatalf("edit must preserve worker scoping: %+v", rec.Workers)
	}
	// unknown id → 404
	if doJSON(t, ch, "PUT", "/api/v1/secrets/sec_nope", map[string]any{"name": "a"}).Code != 404 {
		t.Fatal("edit missing should 404")
	}
}

func TestWorkerAPIIsNamespacedAndControlRejectsWorkers(t *testing.T) {
	s := testServer(t)
	s.DenyPeer = func(string) bool { return true }
	ch := s.ControlHandler()
	denied := doJSON(t, ch, "GET", "/api/v1/secrets", nil)
	if denied.Code != 403 {
		t.Fatalf("control should 403 workers: %d %s", denied.Code, denied.Body.String())
	}
	s.DenyPeer = nil
	wh := s.WorkerHandler()
	if doJSON(t, wh, "GET", "/api/v1/secrets", nil).Code != 404 {
		t.Fatal("worker mux must not serve host APIs")
	}
	if doJSON(t, ch, "POST", "/api/v1/worker/planning", map[string]any{"worker": "piso-worker-demo"}).Code != 404 {
		t.Fatal("control mux must not serve worker APIs")
	}
	createOnCtrl := doJSON(t, ch, "POST", "/api/v1/ingress/requests", map[string]any{"worker": "piso-worker-demo"})
	if createOnCtrl.Code == 200 || createOnCtrl.Code == 201 {
		t.Fatalf("control must not create routes for the worker: %d %s", createOnCtrl.Code, createOnCtrl.Body.String())
	}
	got := doJSON(t, wh, "GET", "/api/v1/worker/health", nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"ok":true`) {
		t.Fatalf("worker health %d %s", got.Code, got.Body.String())
	}
}

func TestNetworkPlusOne(t *testing.T) {
	_, n, err := net.ParseCIDR("172.21.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if got := networkPlusOne(n).String(); got != "172.21.0.1" {
		t.Fatalf("got %s", got)
	}
}

func TestApexRouteRedirectsToSubdomain(t *testing.T) {
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
	if err := s.Store.AddRoute(store.RouteRec{ID: "r1", Name: "plan-demo", Worker: "127.0.0.1", Port: port, Origin: store.AutoRouteOriginExpose}); err != nil {
		t.Fatal(err)
	}
	h := s.WebHandler()

	// apex ?route=plan-demo → 302 to the canonical portless subdomain (the
	// hosts daemon resolves it; no cookie is set anymore — a stale route
	// cookie must never hijack the dashboard).
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://piso.local/?route=plan-demo", nil)
	req.Host = "piso.local"
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("redirect %d", w.Code)
	}
	if n := len(w.Result().Cookies()); n != 0 {
		t.Fatalf("no cookie should be set after option-B redirect, got %d", n)
	}

	// the subdomain (what the browser lands on) proxies to the worker
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "http://plan-demo.piso.local/", nil)
	req2.Host = "plan-demo.piso.local"
	h.ServeHTTP(w2, req2)
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), "from-worker") {
		t.Fatalf("subdomain route %d %s", w2.Code, w2.Body.String())
	}

	// a plain apex is ALWAYS the dashboard, even with a route cookie present
	// (regression: stale cookie used to 404 piso.local).
	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "http://piso.local/", nil)
	req3.Host = "piso.local"
	req3.AddCookie(&http.Cookie{Name: ingressRouteCookie, Value: "plan-demo"})
	h.ServeHTTP(w3, req3)
	if w3.Code != 200 || !strings.Contains(w3.Body.String(), "piso gateway") {
		t.Fatalf("apex with cookie must serve dashboard, got %d %s", w3.Code, w3.Body.String())
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

func TestWebHandlerDispatchesByHost(t *testing.T) {
	s := testServer(t)
	// a backend to proxy to (the "worker")
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
	if err := s.Store.AddRoute(store.RouteRec{ID: "r1", Name: "demo-8080", Worker: "127.0.0.1", Port: port, Origin: store.AutoRouteOriginAuto}); err != nil {
		t.Fatal(err)
	}
	h := s.WebHandler()

	// a bare <label>.piso.local opens the worker server — no port, no redirect
	req := httptest.NewRequest("GET", "http://demo-8080.piso.local/app", nil)
	req.Host = "demo-8080.piso.local"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "from-worker") {
		t.Fatalf("subdomain should proxy, got %d %s", w.Code, w.Body.String())
	}

	// the apex is the dashboard
	apex := httptest.NewRequest("GET", "http://piso.local/", nil)
	apex.Host = "piso.local"
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, apex)
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), "piso gateway") {
		t.Fatalf("apex should serve dashboard, got %d %s", w2.Code, w2.Body.String())
	}

	// unknown subdomain → 404 (no accidental wildcard routing)
	unknown := httptest.NewRequest("GET", "http://nope-99999.piso.local/", nil)
	unknown.Host = "nope-99999.piso.local"
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, unknown)
	if w3.Code == 200 {
		t.Fatalf("unknown subdomain should 404, got %d %s", w3.Code, w3.Body.String())
	}

	// apex ?route= still drives the route (cookie flow)
	qr := httptest.NewRequest("GET", "http://piso.local/?route=demo-8080", nil)
	qr.Host = "piso.local"
	w4 := httptest.NewRecorder()
	h.ServeHTTP(w4, qr)
	if w4.Code != http.StatusFound {
		t.Fatalf("apex ?route= should redirect to drop the query, got %d %s", w4.Code, w4.Body.String())
	}
}
