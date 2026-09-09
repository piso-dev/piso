package store

import (
	"path/filepath"
	"testing"
)

func TestInsertAndQueryActivities(t *testing.T) {
	st := testStore(t)
	if err := st.InsertActivity(Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: ActivityKindProgress, Text: "Started OAuth flow"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertActivity(Activity{Worker: "piso-worker-monitor", Slug: "monitor", Kind: ActivityKindPoke, TargetSlug: "demo", Text: "demo idle 3h?"}); err != nil {
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
	if err := st.InsertActivity(Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: ActivityKindWaiting, Text: "awaiting input"}); err != nil {
		t.Fatal(err)
	}
	all, err := st.QueryActivities(ActivityFilter{Kind: ActivityKindWaiting, Limit: 10})
	if err != nil || len(all) != 1 {
		t.Fatalf("waiting: %+v err=%v", all, err)
	}
}

func TestDeleteOwnActivityScopesToWorker(t *testing.T) {
	st := testStore(t)
	if err := st.InsertActivity(Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: ActivityKindNote, Text: "note"}); err != nil {
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
	if err := st.InsertActivity(Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: ActivityKindReminder, Text: "check PR by 5pm"}); err != nil {
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
	if err := st.InsertActivity(Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: "bogus", Text: "x"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}