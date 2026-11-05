package mcp

import (
	"encoding/json"
	"net/url"
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
	if len(rows) != 1 || !strings.Contains(rows[0].Text, "git clone") || !strings.Contains(rows[0].Text, "./repo") {
		t.Fatalf("%+v", rows)
	}
}

// hostRequestHandler wires a fake coordinator and returns the last call it saw.
type hostCall struct {
	path string
	body map[string]any
}

func hostRequestHandler(t *testing.T, status int, reply string, err error) (*Handler, *store.Store, *hostCall) {
	t.Helper()
	st := activityStore(t)
	h := New(st, nil, t.TempDir())
	got := &hostCall{}
	h.CoordinatorDo = func(method, path string, query url.Values, body []byte) (int, []byte, error) {
		got.path = path
		got.body = map[string]any{}
		_ = json.Unmarshal(body, &got.body)
		if err != nil {
			return 0, nil, err
		}
		return status, []byte(reply), nil
	}
	return h, st, got
}

// The tracked ask is the point of the tool: a host request a human can answer,
// carrying the command and the rendezvous path rather than free text.
func TestHostRequestFilesTrackedAsk(t *testing.T) {
	h, st, got := hostRequestHandler(t, 201, `{"id":"h1","status":"requested"}`, nil)
	caller := store.WorkerRec{PrincipalID: "prin-1", Name: "piso-worker-demo", Slug: "demo"}
	out, rpcErr := h.toolHostRequest(caller, json.RawMessage(
		`{"command":"npm run migrate","resultPath":"/workspace/migrate.log"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	res := out.(map[string]any)
	if res["humanActionId"] != "h1" || res["status"] != "requested" {
		t.Fatalf("%v", res)
	}
	if got.path != "/api/v1/human-actions" {
		t.Fatalf("path %q", got.path)
	}
	if got.body["category"] != "host" || got.body["instructions"] != "npm run migrate" ||
		got.body["verification"] != "./migrate.log" {
		t.Fatalf("%v", got.body)
	}
	if _, ok := got.body["taskId"]; ok {
		t.Fatalf("untasked form must not send taskId: %v", got.body)
	}
	// Both writes happen, and the board row points at the ask.
	rows, _ := st.QueryActivities(store.ActivityFilter{Slug: "demo", Kind: "host", Limit: 1})
	if len(rows) != 1 {
		t.Fatalf("%+v", rows)
	}
	if rows[0].AskID != "h1" {
		t.Fatalf("activity not linked to ask: %+v", rows[0])
	}
	if res["activityId"] != rows[0].ID {
		t.Fatalf("activityId %v != %v", res["activityId"], rows[0].ID)
	}
}

func TestHostRequestStatesPathsTheHostCanUse(t *testing.T) {
	caller := store.WorkerRec{PrincipalID: "prin-1", Name: "piso-worker-demo", Slug: "demo"}
	route := func(t *testing.T, args map[string]any) (map[string]any, *rpcError) {
		t.Helper()
		h, _, got := hostRequestHandler(t, 201, `{"id":"h1","status":"requested"}`, nil)
		raw, _ := json.Marshal(args)
		if _, rpcErr := h.toolHostRequest(caller, raw); rpcErr != nil {
			return nil, rpcErr
		}
		return got.body, nil
	}
	// The worker's own name for the project root is the same directory to the
	// host, so it is rewritten instead of shipped: a person cannot run
	// /workspace/... and reads it as a path that does not exist.
	body, rpcErr := route(t, map[string]any{"command": "npm run migrate", "resultPath": "/workspace/migrate.log"})
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if body["verification"] != "./migrate.log" {
		t.Fatalf("verification %q", body["verification"])
	}
	// A project-relative path already in the host's form is left alone, and the
	// bare root becomes "." rather than an empty string.
	for in, want := range map[string]string{"./.piso/host-actions/result.txt": "./.piso/host-actions/result.txt", "out.log": "out.log", "/workspace": "."} {
		body, rpcErr = route(t, map[string]any{"command": "make test", "resultPath": in})
		if rpcErr != nil {
			t.Fatalf("%s: %s", in, rpcErr.Message)
		}
		if body["verification"] != want {
			t.Fatalf("%s became %q, want %q", in, body["verification"], want)
		}
	}
	// A host script path obeys the same rule, since it is also something a
	// person has to open.
	body, rpcErr = route(t, map[string]any{
		"explanation": "Run from the project root.", "resultPath": "./out.txt",
		"script": map[string]any{"kind": "file", "language": "bash", "path": "/workspace/.piso/host-actions/action.sh"},
	})
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if script := body["script"].(map[string]any); script["path"] != "./.piso/host-actions/action.sh" {
		t.Fatalf("script path %q", script["path"])
	}
	// Anything the host would have to guess is refused with the rule, not
	// silently sent and discovered missing later.
	for _, bad := range []map[string]any{
		{"command": "make test", "resultPath": "/etc/passwd"},
		{"command": "make test", "resultPath": "../outside/result.txt"},
		{"command": "make test", "resultPath": "a/../../b.txt"},
		{"explanation": "Run.", "resultPath": "./out.txt",
			"script": map[string]any{"kind": "file", "language": "bash", "path": "/opt/host/action.sh"}},
	} {
		if _, rpcErr := route(t, bad); rpcErr == nil {
			t.Fatalf("accepted an unusable path: %v", bad)
		} else if !strings.Contains(rpcErr.Message, "project-relative") && !strings.Contains(rpcErr.Message, "inside the project") {
			t.Fatalf("unhelpful refusal %q for %v", rpcErr.Message, bad)
		}
	}
}

func TestHostScriptForwardedWithoutTruncation(t *testing.T) {
	h, _, got := hostRequestHandler(t, 201, `{"id":"h1","status":"requested"}`, nil)
	caller := store.WorkerRec{PrincipalID: "prin-1", Name: "piso-worker-demo", Slug: "demo"}
	contents := "#!/usr/bin/env bash\n" + strings.Repeat("echo hello\n", 300) + "# end\n"
	args, _ := json.Marshal(map[string]any{
		"script":      HostScript{Kind: "inline", Filename: "action.sh", Language: "bash", Contents: contents},
		"explanation": "Save as action.sh; run bash action.sh on the host.", "resultPath": "/workspace/result.log",
	})
	_, rpcErr := h.toolHostRequest(caller, args)
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if got.body["script"].(map[string]any)["contents"] != contents || got.body["explanation"] == nil {
		t.Fatalf("script or explanation altered: %v", got.body)
	}
	for _, script := range []HostScript{
		{Kind: "inline", Filename: "x.sh", Language: "bash", Contents: strings.Repeat("x", 20001)},
		{Kind: "file", Language: "bash"},
		{Kind: "file", Language: "bash", Path: "x.sh", Contents: "unexpected"},
	} {
		args, _ = json.Marshal(map[string]any{"script": script, "explanation": "Run on host", "resultPath": "/workspace/out"})
		_, rpcErr = h.toolHostRequest(caller, args)
		if rpcErr == nil {
			t.Fatalf("accepted invalid script: %+v", script)
		}
	}
}

func TestHostRequestTaskLinkedCarriesTheWait(t *testing.T) {
	// mutate_worker's response: ``id`` is the task, ``humanActionId`` the ask.
	h, st, got := hostRequestHandler(t, 200, `{"id":"t1","attemptId":"a1","status":"waiting","humanActionId":"h1","waitId":"w1"}`, nil)
	caller := store.WorkerRec{PrincipalID: "prin-1", Name: "piso-worker-demo", Slug: "demo"}
	out, rpcErr := h.toolHostRequest(caller, json.RawMessage(`{"command":"add dns","resultPath":"/workspace/dns.txt",`+
		`"taskId":"t1","attemptId":"a1","requestId":"r1","summary":"blocked","checkpoint":"cp"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	res := out.(map[string]any)
	if res["waitId"] != "w1" || res["humanActionId"] != "h1" || res["taskId"] != "t1" {
		t.Fatalf("%v", res)
	}
	for _, key := range []string{"taskId", "attemptId", "requestId", "summary", "checkpoint"} {
		if got.body[key] == nil || got.body[key] == "" {
			t.Fatalf("%s not forwarded: %v", key, got.body)
		}
	}
	// The timeline row must point at the ask, not at the task it blocks: the
	// coordinator answers a task-linked ask with the task id under ``id``.
	rows, _ := st.QueryActivities(store.ActivityFilter{Slug: "demo", Kind: "host", Limit: 1})
	if len(rows) != 1 || rows[0].AskID != "h1" {
		t.Fatalf("activity not linked to the ask: %+v", rows)
	}
	if !strings.Contains(rows[0].Text, "ask h1") {
		t.Fatalf("board text names no ask: %q", rows[0].Text)
	}
}

// A blocking ask that silently degraded to a board line would let a worker exit
// believing it is blocked while nothing holds the task.
func TestHostRequestTaskLinkedRequiresFieldsAndWritesNothing(t *testing.T) {
	h, st, _ := hostRequestHandler(t, 201, `{"id":"h1"}`, nil)
	caller := store.WorkerRec{PrincipalID: "prin-1", Name: "piso-worker-demo", Slug: "demo"}
	_, rpcErr := h.toolHostRequest(caller, json.RawMessage(
		`{"command":"add dns","resultPath":"/workspace/dns.txt","taskId":"t1","requestId":"r1"}`))
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "attemptId") {
		t.Fatalf("%v", rpcErr)
	}
	if rows, _ := st.QueryActivities(store.ActivityFilter{Slug: "demo", Limit: 5}); len(rows) != 0 {
		t.Fatalf("wrote an activity for a rejected ask: %+v", rows)
	}
}

func TestHostRequestUnpairedDegradesOnlyUntasked(t *testing.T) {
	// Task-linked: hard failure, no activity row.
	h, st, _ := hostRequestHandler(t, 0, ``, nil)
	h.CoordinatorDo = nil
	caller := store.WorkerRec{PrincipalID: "prin-1", Name: "piso-worker-demo", Slug: "demo"}
	_, rpcErr := h.toolHostRequest(caller, json.RawMessage(`{"command":"add dns","resultPath":"/workspace/dns.txt",`+
		`"taskId":"t1","attemptId":"a1","requestId":"r1","summary":"blocked","checkpoint":"cp"}`))
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "unpaired") {
		t.Fatalf("%v", rpcErr)
	}
	if rows, _ := st.QueryActivities(store.ActivityFilter{Slug: "demo", Limit: 5}); len(rows) != 0 {
		t.Fatalf("wrote an activity for an unpaired blocking ask: %+v", rows)
	}

	// Untasked: the board line still lands, flagged untracked.
	out, rpcErr := h.toolHostRequest(caller, json.RawMessage(`{"command":"add dns","resultPath":"/workspace/dns.txt"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	res := out.(map[string]any)
	if res["status"] != "untracked" || res["warning"] == nil {
		t.Fatalf("%v", res)
	}
	if rows, _ := st.QueryActivities(store.ActivityFilter{Slug: "demo", Limit: 5}); len(rows) != 1 {
		t.Fatalf("%+v", rows)
	}
}

// The ask is the block. Losing the timeline trace must not undo it.
func TestHostRequestBoardFailureKeepsTheAsk(t *testing.T) {
	h, st, _ := hostRequestHandler(t, 200, `{"id":"t1","status":"waiting","humanActionId":"h1","waitId":"w1"}`, nil)
	caller := store.WorkerRec{PrincipalID: "prin-1", Name: "piso-worker-demo", Slug: "demo"}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	out, rpcErr := h.toolHostRequest(caller, json.RawMessage(`{"command":"add dns","resultPath":"/workspace/dns.txt",`+
		`"taskId":"t1","attemptId":"a1","requestId":"r1","summary":"blocked","checkpoint":"cp"}`))
	if rpcErr != nil {
		t.Fatalf("a lost board line must not fail the blocking ask: %v", rpcErr.Message)
	}
	res := out.(map[string]any)
	if res["humanActionId"] != "h1" || res["warning"] == nil {
		t.Fatalf("%v", res)
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

// The board row is capped at 500 chars by insertBoardAsk, so the ask reference
// has to fit inside the budget: a truncated link is a row that points nowhere.
func TestHostRequestBoardTextKeepsTheAskReference(t *testing.T) {
	h, st, _ := hostRequestHandler(t, 200, `{"id":"t1","status":"waiting","humanActionId":"`+
		strings.Repeat("a", 36)+`","waitId":"w1"}`, nil)
	caller := store.WorkerRec{PrincipalID: "prin-1", Name: "piso-worker-demo", Slug: "demo"}
	long := strings.Repeat("x", 900)
	_, rpcErr := h.toolHostRequest(caller, json.RawMessage(`{"command":"`+long+`","resultPath":"/workspace/`+
		long+`","taskId":"t1","attemptId":"a1","requestId":"r1","summary":"blocked","checkpoint":"cp"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	rows, _ := st.QueryActivities(store.ActivityFilter{Slug: "demo", Kind: "host", Limit: 1})
	if len(rows) != 1 {
		t.Fatalf("%+v", rows)
	}
	if !strings.Contains(rows[0].Text, "ask "+strings.Repeat("a", 36)) {
		t.Fatalf("ask reference truncated out of the board row: %q", rows[0].Text)
	}
	if len([]rune(rows[0].Text)) > 500 {
		t.Fatalf("board row over the cap: %d", len([]rune(rows[0].Text)))
	}
}
