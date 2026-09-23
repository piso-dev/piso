package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRoutesEmptyJSONIsArray(t *testing.T) {
	st := testStore(t)
	got := st.Routes()
	if got == nil {
		t.Fatal("Routes() must not return nil on a fresh store")
	}
	if len(got) != 0 {
		t.Fatalf("expected no routes, got %+v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "[]" {
		t.Fatalf("JSON want [] got %s", raw)
	}
}

func TestUpsertPendingIngressIdempotent(t *testing.T) {
	st := testStore(t)
	first, created, err := st.UpsertPendingIngress(IngressRequestRec{
		ID: "ing_1", Kind: IngressKindPlanning, Worker: "piso-worker-demo",
		Slug: "demo", Port: 19432, Name: "plan-demo",
	})
	if err != nil || !created || first.ID != "ing_1" {
		t.Fatalf("first: created=%v rec=%+v err=%v", created, first, err)
	}
	second, created, err := st.UpsertPendingIngress(IngressRequestRec{
		ID: "ing_2", Kind: IngressKindPlanning, Worker: "piso-worker-demo",
		Slug: "demo", Port: 19432, Name: "plan-demo",
	})
	if err != nil || created {
		t.Fatalf("second should refresh: created=%v err=%v", created, err)
	}
	if second.ID != first.ID {
		t.Fatalf("id changed %s → %s", first.ID, second.ID)
	}
	if len(st.PendingIngress()) != 1 {
		t.Fatalf("pending %d", len(st.PendingIngress()))
	}
}

func TestApproveIngressCreatesRouteAndClearsPending(t *testing.T) {
	st := testStore(t)
	_, _, err := st.UpsertPendingIngress(IngressRequestRec{
		ID: "ing_a", Kind: IngressKindPlanning, Worker: "piso-worker-demo",
		Port: 19432, Name: "plan-demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, route, err := st.ApproveIngress("ing_a", "route_a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != "ing_a" || route.Name != "plan-demo" || route.Worker != "piso-worker-demo" || route.Port != 19432 || route.Origin != AutoRouteOriginPlan || route.LastSeenMs <= 0 {
		t.Fatalf("rec=%+v route=%+v", rec, route)
	}
	if len(st.PendingIngress()) != 0 {
		t.Fatal("pending should be empty after approve")
	}
	got, ok := st.RouteByName("plan-demo")
	if !ok || got.ID != "route_a" {
		t.Fatalf("route: %+v ok=%v", got, ok)
	}
}

// TestPlanRouteSweptByGC: an approved plan route is Origin=plan, so the lazy
// stale sweep in Routes() drops it once it ages past AutoRouteGraceMs with no
// heartbeat (the planning port is never watcher-reported).
func TestPlanRouteSweptByGC(t *testing.T) {
	st := testStore(t)
	if _, _, err := st.UpsertPendingIngress(IngressRequestRec{
		ID: "ing_gc", Kind: IngressKindPlanning, Worker: "piso-worker-demo", Port: 19432, Name: "plan-demo",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ApproveIngress("ing_gc", "route_gc"); err != nil {
		t.Fatal(err)
	}
	routes := st.Routes()
	if len(routes) != 1 || routes[0].Name != "plan-demo" || routes[0].Origin != AutoRouteOriginPlan {
		t.Fatalf("expected one plan route, got %+v", routes)
	}
	// Age the route past the grace and confirm the next Routes() sweeps it.
	aged := routes[0].LastSeenMs + AutoRouteGraceMs + 1
	orig := timeNowMs
	timeNowMs = func() int64 { return aged }
	defer func() { timeNowMs = orig }()
	if got := st.Routes(); len(got) != 0 {
		t.Fatalf("expected plan route swept by GC, got %+v", got)
	}
}

// TestCancelRemovesLivePlanRouteOnly: cancel tears down the worker's live
// plan route but leaves its dev-server auto routes (and expose routes)
// untouched — the plan- prefix guard must not catch demo-9090.
func TestCancelRemovesLivePlanRouteOnly(t *testing.T) {
	st := testStore(t)
	if _, _, err := st.UpsertPendingIngress(IngressRequestRec{
		ID: "ing_c", Kind: IngressKindPlanning, Worker: "piso-worker-demo", Slug: "demo", Port: 19432, Name: "plan-demo",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ApproveIngress("ing_c", "route_c"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncWorkerPorts("piso-worker-demo", "demo", []int{9090}); err != nil {
		t.Fatal(err)
	}
	n := st.CancelPendingIngress("piso-worker-demo", IngressKindPlanning)
	if n != 1 {
		t.Fatalf("expected 1 removal (live plan route), got %d", n)
	}
	got := st.Routes()
	if len(got) != 1 || got[0].Name != "demo-9090" || got[0].Origin != AutoRouteOriginAuto {
		t.Fatalf("expected only auto route demo-9090 to survive, got %+v", got)
	}
}

func TestApproveIngressRejectsForeignLabel(t *testing.T) {
	st := testStore(t)
	if err := st.AddRoute(RouteRec{ID: "route_x", Name: "plan-demo", Worker: "piso-worker-other", Port: 19432}); err != nil {
		t.Fatal(err)
	}
	_, _, err := st.UpsertPendingIngress(IngressRequestRec{
		ID: "ing_b", Kind: IngressKindPlanning, Worker: "piso-worker-demo",
		Port: 19432, Name: "plan-demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.ApproveIngress("ing_b", "route_b")
	if !errors.Is(err, ErrIngressLabelTaken) {
		t.Fatalf("want label taken, got %v", err)
	}
	if len(st.PendingIngress()) != 1 {
		t.Fatal("pending should remain after failed approve")
	}
}

func TestDismissAndCancelPending(t *testing.T) {
	st := testStore(t)
	_, _, err := st.UpsertPendingIngress(IngressRequestRec{
		ID: "ing_c", Kind: IngressKindPlanning, Worker: "piso-worker-demo",
		Port: 19432, Name: "plan-demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DismissIngress("ing_c"); err != nil {
		t.Fatal(err)
	}
	if len(st.PendingIngress()) != 0 {
		t.Fatal("dismiss should drop pending")
	}
	_, _, err = st.UpsertPendingIngress(IngressRequestRec{
		ID: "ing_d", Kind: IngressKindPlanning, Worker: "piso-worker-demo",
		Port: 19432, Name: "plan-demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := st.CancelPendingIngress("piso-worker-demo", IngressKindPlanning); n != 1 {
		t.Fatalf("cancelled %d", n)
	}
	if len(st.PendingIngress()) != 0 {
		t.Fatal("cancel should drop pending")
	}
	if err := st.DismissIngress("missing"); !errors.Is(err, ErrIngressNotFound) {
		t.Fatalf("missing dismiss: %v", err)
	}
}

func TestAutoRouteName(t *testing.T) {
	if got := AutoRouteName("My_Project", 8080); got != "my-project-8080" {
		t.Fatalf("got %q", got)
	}
	if got := AutoRouteName("demo", 80); got != "demo-80" {
		t.Fatalf("got %q", got)
	}
	// 63-char cap: long slug + port must not exceed, no trailing dash.
	long := AutoRouteName(strings.Repeat("a", 63), 99999)
	if len(long) > 63 || strings.HasSuffix(long, "-") {
		t.Fatalf("long label %q (len %d)", long, len(long))
	}
}

func TestSyncWorkerPortsCreatesAndRefreshes(t *testing.T) {
	st := testStore(t)
	created, err := st.SyncWorkerPorts("piso-worker-demo", "demo", []int{9090, 8080})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 {
		t.Fatalf("created %d", len(created))
	}
	r8080, ok := st.RouteByName("demo-8080")
	if !ok || r8080.Worker != "piso-worker-demo" || r8080.Port != 8080 || r8080.Origin != AutoRouteOriginAuto {
		t.Fatalf("route %+v ok=%v", r8080, ok)
	}
	if r8080.LastSeenMs <= 0 {
		t.Fatal("auto route missing heartbeat")
	}
	// a second report is a refresh, not a duplicate (id + last seen updated)
	again, err := st.SyncWorkerPorts("piso-worker-demo", "demo", []int{8080, 9090})
	if err != nil || len(again) != 0 {
		t.Fatalf("second report created %d err=%v", len(again), err)
	}
	r2, _ := st.RouteByName("demo-8080")
	if r2.ID != r8080.ID || r2.LastSeenMs < r8080.LastSeenMs {
		t.Fatalf("refresh lost identity or heartbeat: %+v vs %+v", r2, r8080)
	}
	// another worker cannot share the label namespace (its own slug)
	other, err := st.SyncWorkerPorts("piso-worker-other", "other", []int{8080})
	if err != nil || len(other) != 1 || other[0].Name != "other-8080" {
		t.Fatalf("other worker %+v err=%v", other, err)
	}
}

func TestSyncWorkerPortsExposeWins(t *testing.T) {
	st := testStore(t)
	if err := st.AddRoute(RouteRec{ID: "route_x", Name: "demo-8080", Worker: "piso-worker-demo", Port: 8080, Origin: AutoRouteOriginExpose}); err != nil {
		t.Fatal(err)
	}
	created, err := st.SyncWorkerPorts("piso-worker-demo", "demo", []int{8080})
	if err != nil || len(created) != 0 {
		t.Fatalf("expose route must not be recreated: %d err=%v", len(created), err)
	}
	r, ok := st.RouteByName("demo-8080")
	if !ok || r.ID != "route_x" || r.Origin != AutoRouteOriginExpose {
		t.Fatalf("expose route replaced: %+v", r)
	}
	// expose route still gets the heartbeat badge refresh
	if r.LastSeenMs <= 0 {
		t.Fatal("expose route missing heartbeat refresh")
	}
}

func TestSyncWorkerPortsRateCap(t *testing.T) {
	st := testStore(t)
	ports := make([]int, 25)
	for i := 0; i < 25; i++ {
		ports[i] = 1000 + i
	}
	created, err := st.SyncWorkerPorts("piso-worker-demo", "demo", ports)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != MaxAutoRoutesPerWorker {
		t.Fatalf("rate cap: created %d", len(created))
	}
	n := 0
	for _, r := range st.Routes() {
		if r.Origin == AutoRouteOriginAuto && r.Worker == "piso-worker-demo" {
			n++
		}
	}
	if n != MaxAutoRoutesPerWorker {
		t.Fatalf("auto routes after cap: %d", n)
	}
}

func TestRoutesSweepsStaleAuto(t *testing.T) {
	st := testStore(t)
	now := time.Now().UnixNano()/1000000
	if err := st.AddRoute(RouteRec{ID: "r_stale", Name: "demo-8080", Worker: "piso-worker-demo", Port: 8080, Origin: AutoRouteOriginAuto, LastSeenMs: now - AutoRouteGraceMs - 1000}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRoute(RouteRec{ID: "r_fresh", Name: "demo-9090", Worker: "piso-worker-demo", Port: 9090, Origin: AutoRouteOriginAuto, LastSeenMs: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRoute(RouteRec{ID: "r_exp", Name: "preview", Worker: "piso-worker-demo", Port: 5173, Origin: AutoRouteOriginExpose}); err != nil {
		t.Fatal(err)
	}
	routes := st.Routes()
	n := 0
	for _, r := range routes {
		if r.ID == "r_stale" {
			t.Fatal("stale auto route survived the sweep")
		}
		n++
	}
	if n != 2 {
		t.Fatalf("sweep kept %d routes (want fresh + expose)", n)
	}
	// the sweep persisted: a reload sees the stale route gone
	got, ok := st.RouteByName("demo-8080")
	if ok || got.ID != "" {
		t.Fatalf("stale route still resolvable: %+v", got)
	}
}

func TestSetWorkerUnreachableHints(t *testing.T) {
	st := testStore(t)
	st.SetWorkerUnreachable("piso-worker-demo", []UnreachablePort{
		UnreachablePort{Port: 19432, Note: "bound to loopback 127.0.0.1"},
	})
	hints, ok := st.WorkerUnreachable("piso-worker-demo")
	if !ok || len(hints) != 1 || hints[0].Port != 19432 {
		t.Fatalf("hints %+v ok=%v", hints, ok)
	}
	st.SetWorkerUnreachable("piso-worker-demo", nil)
	if _, ok := st.WorkerUnreachable("piso-worker-demo"); ok {
		t.Fatal("cleared hints still present")
	}
}

func TestSetRouteDisabledKeepsRowAndFlips(t *testing.T) {
	st := testStore(t)
	if err := st.AddRoute(RouteRec{ID: "r1", Name: "demo-8080", Worker: "piso-worker-demo", Port: 8080, Origin: AutoRouteOriginAuto, LastSeenMs: time.Now().UnixNano()/1000000}); err != nil {
		t.Fatal(err)
	}
	// flip off
	rec, err := st.SetRouteDisabled("r1", true)
	if err != nil || !rec.Disabled {
		t.Fatalf("disable %+v err=%v", rec, err)
	}
	// row still resolvable (the toggle can re-enable)
	got, ok := st.RouteByName("demo-8080")
	if !ok || !got.Disabled {
		t.Fatalf("row lost or not marked: %+v ok=%v", got, ok)
	}
	// re-enable
	rec, err = st.SetRouteDisabled("r1", false)
	if err != nil || rec.Disabled {
		t.Fatalf("enable %+v err=%v", rec, err)
	}
	// unknown id → ErrRouteNotFound
	if _, err := st.SetRouteDisabled("nope", true); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("missing route: %v", err)
	}
}
