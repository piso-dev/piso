package store

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestProjectVisibilityAndRemoval(t *testing.T) {
	st := workerTestStore(t)
	for _, rec := range []WorkerRec{
		{Name: "piso-worker-demo", Slug: "demo", IPs: []string{"192.168.107.50"}},
		{Name: "piso-worker-demo-two", Slug: "demo"},
		{Name: "piso-worker-other", Slug: "other"},
	} {
		if _, err := st.UpsertWorker(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetProjectHidden("missing", true); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := st.SetProjectHidden("demo", true); err != nil {
		t.Fatal(err)
	}
	// Registration refresh must not undo a user's visibility preference.
	if _, err := st.UpsertWorker(WorkerRec{Name: "piso-worker-demo", Slug: "demo", IPs: []string{"192.168.107.50"}}); err != nil {
		t.Fatal(err)
	}
	for _, rec := range st.Workers() {
		if rec.ProjectHidden != (rec.Slug == "demo") {
			t.Fatalf("visibility: %+v", rec)
		}
	}
	// Verify persistence, not just the in-memory state.
	disk, err := os.ReadFile(st.path)
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(disk, &state); err != nil {
		t.Fatal(err)
	}
	if !state.Workers[0].ProjectHidden {
		t.Fatal("hidden preference not saved")
	}
	if err := st.SetProjectHidden("demo", false); err != nil {
		t.Fatal(err)
	}
	for _, rec := range st.Workers() {
		if rec.ProjectHidden {
			t.Fatal("restore failed")
		}
	}
	for i := 0; i < 2; i++ {
		if err := st.DeleteWorker("piso-worker-demo"); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := st.WorkerByName("piso-worker-demo"); ok {
		t.Fatal("worker still registered")
	}
	if _, ok := st.WorkerByIP("192.168.107.50"); ok {
		t.Fatal("old IP still registered")
	}
	if len(st.Workers()) != 2 {
		t.Fatal("unrelated worker removed")
	}
	disk, err = os.ReadFile(st.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(disk, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Workers) != 2 {
		t.Fatal("removal not saved")
	}
	if _, err := st.UpsertWorker(WorkerRec{Name: "piso-worker-demo", Slug: "demo"}); err != nil {
		t.Fatal(err)
	}
	if len(st.Workers()) != 3 {
		t.Fatal("piso up failed to restore registration")
	}
}

func TestRemoteSyncPreservesProjectVisibility(t *testing.T) {
	st := workerTestStore(t)
	rows := []WorkerRec{{Name: "piso-worker-remote", Slug: "remote"}}
	if err := st.ReplaceRemoteWorkers(rows); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProjectHidden("remote", true); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceRemoteWorkers(rows); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.WorkerByName("piso-worker-remote")
	if !rec.ProjectHidden {
		t.Fatal("sync lost visibility")
	}
}
