package store

import (
	"encoding/json"
	"os"
	"testing"
)

func TestModelOverrideSetReplacesAndDeletes(t *testing.T) {
	st := workerTestStore(t)
	if _, err := st.UpsertWorker(WorkerRec{Name: "piso-worker-demo", Slug: "demo"}); err != nil {
		t.Fatal(err)
	}
	if got := st.ModelOverrideProviderID("demo"); got != "" {
		t.Fatalf("unset override: %q", got)
	}
	if _, err := st.SetModelOverride("demo", "piso"); err != nil {
		t.Fatal(err)
	}
	if got := st.ModelOverrideProviderID("demo"); got != "piso" {
		t.Fatalf("override: %q", got)
	}
	// A second set replaces rather than appends: the list must never say
	// "demo on A and on B".
	if _, err := st.SetModelOverride("demo", "piso-two"); err != nil {
		t.Fatal(err)
	}
	if rows := st.ModelOverrides(); len(rows) != 1 || rows[0].ProviderID != "piso-two" {
		t.Fatalf("override list: %+v", rows)
	}
	// A blank provider is never stored; deleting the row is the only way to
	// mean "follow the account default".
	if _, err := st.SetModelOverride("demo", "  "); err == nil {
		t.Fatal("blank provider accepted")
	}
	if err := st.DeleteModelOverride("demo"); err != nil {
		t.Fatal(err)
	}
	if rows := st.ModelOverrides(); len(rows) != 0 {
		t.Fatalf("after delete: %+v", rows)
	}
	// Deleting an override that is not there is a no-op, not an error.
	if err := st.DeleteModelOverride("demo"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestModelOverridePersists(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	st, err := New(path, dir+"/log.jsonl", dir+"/patterns.json", dir+"/activities.db", 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetModelOverride("demo", "piso-two"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.ModelOverrides) != 1 || state.ModelOverrides[0].Slug != "demo" ||
		state.ModelOverrides[0].ProviderID != "piso-two" {
		t.Fatalf("persisted: %+v", state.ModelOverrides)
	}
	// A fresh store reads the same pin back.
	reopened, err := New(path, dir+"/log.jsonl", dir+"/patterns.json", dir+"/activities.db", 20)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.ModelOverrideProviderID("demo"); got != "piso-two" {
		t.Fatalf("reloaded override: %q", got)
	}
}

// A slug is the key, so an override left behind by a deleted worker would
// silently re-target whatever project claims that slug next.
func TestModelOverridePrunedWithWorker(t *testing.T) {
	st := workerTestStore(t)
	if _, err := st.UpsertWorker(WorkerRec{Name: "piso-worker-demo", Slug: "demo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetModelOverride("demo", "piso"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteWorker("piso-worker-demo"); err != nil {
		t.Fatal(err)
	}
	if rows := st.ModelOverrides(); len(rows) != 0 {
		t.Fatalf("override survived worker deletion: %+v", rows)
	}
}

// An older state.json has no overrides at all, which must mean exactly what it
// meant before the feature: every worker follows the account default.
func TestModelOverridesMigrationKeepsOlderFilesUnchanged(t *testing.T) {
	st := State{Version: 10, Workers: []WorkerRec{{Name: "piso-worker-demo", Slug: "demo"}}}
	if !applyMigrations(&st) {
		t.Fatal("v10 state should migrate")
	}
	if st.Version != CurrentStateVersion {
		t.Fatalf("version = %d, want %d", st.Version, CurrentStateVersion)
	}
	if st.ModelOverrides == nil {
		t.Fatal("modelOverrides should be an empty list, not null")
	}
	if len(st.ModelOverrides) != 0 {
		t.Fatalf("migration invented rows: %+v", st.ModelOverrides)
	}
}
