package mcp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"piso/gateway/internal/store"
)

func activityStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(
		filepath.Join(dir, "state.json"),
		filepath.Join(dir, "log.jsonl"),
		filepath.Join(dir, "patterns.json"),
		filepath.Join(dir, "activities.db"),
		20,
	)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestActivityPostMatchesBoardRow(t *testing.T) {
	st := activityStore(t)
	h := New(st, nil, t.TempDir())
	caller := store.WorkerRec{Name: "piso-worker-demo", Slug: "demo"}
	out, rpcErr := h.toolActivityPost(caller, json.RawMessage(`{"kind":"progress","text":"Started OAuth PKCE flow"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	got := out.(map[string]any)
	id, _ := got["id"].(string)
	if id == "" || got["kind"] != "progress" {
		t.Fatalf("%v", got)
	}
	rows, err := st.QueryActivities(store.ActivityFilter{Slug: "demo", Limit: 5})
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	if rows[0].Worker != "piso-worker-demo" || rows[0].Text != "Started OAuth PKCE flow" {
		t.Fatalf("%+v", rows[0])
	}
}

func TestActivityPostRejectsWorkerArgAndIdle(t *testing.T) {
	st := activityStore(t)
	h := New(st, nil, t.TempDir())
	caller := store.WorkerRec{Name: "piso-worker-demo", Slug: "demo"}
	_, rpcErr := h.toolActivityPost(caller, json.RawMessage(`{"kind":"note","text":"x","worker":"other"}`))
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "rejected") {
		t.Fatalf("%v", rpcErr)
	}
	_, rpcErr = h.toolActivityPost(caller, json.RawMessage(`{"kind":"idle","text":"idle at prompt"}`))
	if rpcErr == nil || rpcErr.Message != "unknown kind" {
		t.Fatalf("%v", rpcErr)
	}
}

func TestActivityPostSanitizesAndPokeTarget(t *testing.T) {
	st := activityStore(t)
	h := New(st, nil, t.TempDir())
	caller := store.WorkerRec{Name: "piso-worker-monitor", Slug: "monitor"}
	_, rpcErr := h.toolActivityPost(caller, json.RawMessage(`{"kind":"poke","text":"demo idle\n3h?","targetSlug":"demo"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	rows, err := st.QueryActivities(store.ActivityFilter{Slug: "monitor", Limit: 1})
	if err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	if rows[0].TargetSlug != "demo" || rows[0].Text != "demo idle 3h?" {
		t.Fatalf("%+v", rows[0])
	}
}

func TestHostRequestWritesHostKind(t *testing.T) {
	st := activityStore(t)
	h := New(st, nil, t.TempDir())
	caller := store.WorkerRec{Name: "piso-worker-demo", Slug: "demo"}
	out, rpcErr := h.toolHostRequest(caller, json.RawMessage(`{"command":"git clone git@github.com:org/repo.git","resultPath":"/workspace/repo"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if out.(map[string]any)["kind"] != "host" {
		t.Fatalf("%v", out)
	}
	rows, _ := st.QueryActivities(store.ActivityFilter{Slug: "demo", Kind: "host", Limit: 1})
	if len(rows) != 1 || !strings.Contains(rows[0].Text, "git clone") || !strings.Contains(rows[0].Text, "/workspace/repo") {
		t.Fatalf("%+v", rows)
	}
}

func TestActivityFeedIsOwnSlugOnly(t *testing.T) {
	st := activityStore(t)
	h := New(st, nil, t.TempDir())
	h.Now = func() time.Time { return time.UnixMilli(10_000) }
	if _, err := st.InsertActivity(store.Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: "note", Text: "mine", Ts: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertActivity(store.Activity{Worker: "piso-worker-other", Slug: "other", Kind: "note", Text: "theirs", Ts: 2000}); err != nil {
		t.Fatal(err)
	}
	out, rpcErr := h.toolActivityFeed(store.WorkerRec{Name: "piso-worker-demo", Slug: "demo"}, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	rows := out.(map[string]any)["rows"].([]map[string]any)
	if len(rows) != 1 || rows[0]["text"] != "mine" {
		t.Fatalf("%v", rows)
	}
	if rows[0]["ageMin"] != int64(0) {
		t.Fatalf("age %v", rows[0]["ageMin"])
	}
}

func TestToolListIncludesBoard(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	names := map[string]bool{}
	for _, tdef := range h.toolList() {
		names[tdef["name"].(string)] = true
	}
	for _, want := range []string{"activity_post", "activity_feed", "host_request"} {
		if !names[want] {
			t.Fatalf("missing %s", want)
		}
	}
	if names["workers_list"] {
		t.Fatal("workers_list must not be listed")
	}
}
