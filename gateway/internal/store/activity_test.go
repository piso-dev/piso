package store

import (
	"path/filepath"
	"testing"
)

func TestInsertAndQueryActivities(t *testing.T) {
	st := testStore(t)
	if _, err := st.InsertActivity(Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: ActivityKindProgress, Text: "Started OAuth flow", Ts: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertActivity(Activity{Worker: "piso-worker-monitor", Slug: "monitor", Kind: ActivityKindPoke, TargetSlug: "demo", Text: "demo idle 3h?", Ts: 2000}); err != nil {
		t.Fatal(err)
	}
	// all, newest first
	all, err := st.QueryActivities(ActivityFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Kind != ActivityKindPoke {
		t.Fatalf("all: %+v", all)
	}
	// by slug
	bySlug, err := st.QueryActivities(ActivityFilter{Slug: "demo", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(bySlug) != 1 || bySlug[0].Kind != ActivityKindProgress {
		t.Fatalf("bySlug: %+v", bySlug)
	}
	// by target (pokes landing on the demo track)
	byTarget, err := st.QueryActivities(ActivityFilter{TargetSlug: "demo", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(byTarget) != 1 || byTarget[0].TargetSlug != "demo" {
		t.Fatalf("byTarget: %+v", byTarget)
	}
	// by kind
	active, err := st.QueryActivities(ActivityFilter{Kind: ActivityKindActive, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active: %+v", active)
	}
}

func TestWaitingKindAccepted(t *testing.T) {
	st := testStore(t)
	if _, err := st.InsertActivity(Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: ActivityKindWaiting, Text: "awaiting input"}); err != nil {
		t.Fatal(err)
	}
	all, err := st.QueryActivities(ActivityFilter{Kind: ActivityKindWaiting, Limit: 10})
	if err != nil || len(all) != 1 {
		t.Fatalf("waiting: %+v err=%v", all, err)
	}
}

func TestIdleKindAccepted(t *testing.T) {
	st := testStore(t)
	if _, err := st.InsertActivity(Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: ActivityKindIdle, Text: "idle at prompt"}); err != nil {
		t.Fatal(err)
	}
	all, err := st.QueryActivities(ActivityFilter{Kind: ActivityKindIdle, Limit: 10})
	if err != nil || len(all) != 1 {
		t.Fatalf("idle: %+v err=%v", all, err)
	}
}

func TestNewestTsByTarget(t *testing.T) {
	st := testStore(t)
	for _, a := range []Activity{
		{Worker: "piso-worker-monitor", Slug: "monitor", Kind: ActivityKindPoke, TargetSlug: "alpha", Text: "poke1", Ts: 1000},
		{Worker: "piso-worker-monitor", Slug: "monitor", Kind: ActivityKindPoke, TargetSlug: "beta", Text: "poke2", Ts: 2000},
		{Worker: "piso-worker-monitor", Slug: "monitor", Kind: ActivityKindNote, TargetSlug: "alpha", Text: "curation", Ts: 3000},
		{Worker: "piso-worker-demo", Slug: "demo", Kind: ActivityKindProgress, Text: "no target", Ts: 500},
	} {
		if _, err := st.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	// per-target newest ts for the monitor's own postings
	m, err := st.NewestTsByTarget("piso-worker-monitor")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m["alpha"] != 3000 || m["beta"] != 2000 {
		t.Fatalf("monitor targets: %+v", m)
	}
	// a worker that never aimed anything at a project has an empty map
	m2, err := st.NewestTsByTarget("piso-worker-demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(m2) != 1 {
		t.Fatalf("demo should only map its own empty target: %+v", m2)
	}
	if _, ok := m2[""]; !ok {
		t.Fatalf("expected empty-target key, got %+v", m2)
	}
	// unknown worker → empty map, no error
	m3, err := st.NewestTsByTarget("piso-worker-nobody")
	if err != nil || len(m3) != 0 {
		t.Fatalf("nobody: %+v err=%v", m3, err)
	}
}

func TestDeleteOwnActivityScopesToWorker(t *testing.T) {
	st := testStore(t)
	if _, err := st.InsertActivity(Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: ActivityKindNote, Text: "note"}); err != nil {
		t.Fatal(err)
	}
	all, _ := st.QueryActivities(ActivityFilter{Limit: 10})
	if len(all) != 1 {
		t.Fatalf("seed: %+v", all)
	}
	id := all[0].ID
	// another worker cannot delete it
	deleted, err := st.DeleteOwnActivity(id, "piso-worker-other")
	if err != nil || deleted {
		t.Fatalf("cross-worker delete: deleted=%v err=%v", deleted, err)
	}
	// the owning worker can
	deleted, err = st.DeleteOwnActivity(id, "piso-worker-demo")
	if err != nil || !deleted {
		t.Fatalf("own delete: deleted=%v err=%v", deleted, err)
	}
	// deleting again → not found
	deleted, _ = st.DeleteOwnActivity(id, "piso-worker-demo")
	if deleted {
		t.Fatal("second delete should be false")
	}
}

func TestActivitiesPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertActivity(Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: ActivityKindReminder, Text: "check PR by 5pm"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// reopen on the same DB path → the reminder survived (durability)
	st2, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	all, err := st2.QueryActivities(ActivityFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Kind != ActivityKindReminder {
		t.Fatalf("persisted: %+v", all)
	}
}

func TestInsertUnknownKindRejected(t *testing.T) {
	st := testStore(t)
	if _, err := st.InsertActivity(Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: "bogus", Text: "x"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}