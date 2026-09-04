package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), 20)
	if err != nil {
		t.Fatal(err)
	}
	return st
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
	if rec.ID != "ing_a" || route.Name != "plan-demo" || route.Worker != "piso-worker-demo" || route.Port != 19432 {
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
