package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func workerTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), 20)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestUpsertWorkerCreates(t *testing.T) {
	st := workerTestStore(t)
	rec, err := st.UpsertWorker(WorkerRec{
		Name: "piso-worker-demo", Slug: "demo", IPs: []string{"192.168.107.50"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Slug != "demo" {
		t.Fatalf("slug %q", rec.Slug)
	}
	byIP, ok := st.WorkerByIP("192.168.107.50")
	if !ok || byIP.Slug != "demo" {
		t.Fatalf("WorkerByIP miss: %+v", byIP)
	}
}

func TestUpsertWorkerRefreshesByName(t *testing.T) {
	st := workerTestStore(t)
	if _, err := st.UpsertWorker(WorkerRec{Name: "piso-worker-demo", Slug: "demo", IPs: []string{"192.168.107.50"}}); err != nil {
		t.Fatal(err)
	}
	// new IP on re-up (DHCP changed); must replace, not duplicate
	rec, err := st.UpsertWorker(WorkerRec{Name: "piso-worker-demo", Slug: "demo", IPs: []string{"192.168.107.99"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.IPs) != 1 || rec.IPs[0] != "192.168.107.99" {
		t.Fatalf("IPs not refreshed: %+v", rec.IPs)
	}
	if len(st.Workers()) != 1 {
		t.Fatalf("workers count %d", len(st.Workers()))
	}
	if _, ok := st.WorkerByIP("192.168.107.50"); ok {
		t.Fatal("old IP still resolves")
	}
}

func TestWorkerByName(t *testing.T) {
	st := workerTestStore(t)
	if _, err := st.UpsertWorker(WorkerRec{Name: "piso-worker-demo", Slug: "demo"}); err != nil {
		t.Fatal(err)
	}
	rec, ok := st.WorkerByName("piso-worker-demo")
	if !ok || rec.Slug != "demo" {
		t.Fatalf("WorkerByName miss: %+v", rec)
	}
	if _, ok := st.WorkerByName("piso-worker-other"); ok {
		t.Fatal("unexpected hit")
	}
}

func TestWorkerBySlug(t *testing.T) {
	st := workerTestStore(t)
	if _, err := st.UpsertWorker(WorkerRec{Name: "piso-worker-demo", Slug: "demo", IPs: []string{"192.168.107.50"}}); err != nil {
		t.Fatal(err)
	}
	rec, ok := st.WorkerBySlug("demo")
	if !ok || rec.Name != "piso-worker-demo" {
		t.Fatalf("WorkerBySlug miss: %+v", rec)
	}
}

func TestSetWorkerInternetPreservesIPsAndSlug(t *testing.T) {
	st := workerTestStore(t)
	if _, err := st.UpsertWorker(WorkerRec{Name: "piso-worker-demo", Slug: "demo", IPs: []string{"192.168.107.50"}}); err != nil {
		t.Fatal(err)
	}
	rec, err := st.SetWorkerInternet("piso-worker-demo", true)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.InternetDisabled {
		t.Fatal("expected disabled")
	}
	if rec.Slug != "demo" || len(rec.IPs) != 1 || rec.IPs[0] != "192.168.107.50" {
		t.Fatalf("slug/IPs not preserved: %+v", rec)
	}
	// flip back
	rec, err = st.SetWorkerInternet("piso-worker-demo", false)
	if err != nil || rec.InternetDisabled {
		t.Fatalf("re-enable: %+v err=%v", rec, err)
	}
	// unknown worker
	if _, err := st.SetWorkerInternet("piso-worker-missing", true); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("expected ErrWorkerNotFound, got %v", err)
	}
}

func TestWorkerRegistryPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	logPath := filepath.Join(dir, "log.jsonl")
	patPath := filepath.Join(dir, "patterns.json")
	st, err := New(path, logPath, patPath, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertWorker(WorkerRec{Name: "piso-worker-demo", Slug: "demo", IPs: []string{"192.168.107.50"}}); err != nil {
		t.Fatal(err)
	}
	// reopen from disk
	st2, err := New(path, logPath, patPath, 20)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := st2.WorkerByIP("192.168.107.50")
	if !ok || rec.Slug != "demo" {
		t.Fatalf("persisted worker lost: %+v", rec)
	}
}

func TestApplyMigrationsV1ToV2(t *testing.T) {
	st := State{}
	st.Version = 1
	st.Workers = nil
	if !applyMigrations(&st) {
		t.Fatal("expected rewrite")
	}
	if st.Version != 2 {
		t.Fatalf("version %d", st.Version)
	}
	if st.Workers == nil {
		t.Fatal("workers should be non-nil after v2 migration")
	}
	if applyMigrations(&st) {
		t.Fatal("v2 should be a no-op")
	}
}