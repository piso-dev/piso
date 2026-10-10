package mcp

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"piso/gateway/internal/store"
	gtwc "piso/gateway/internal/technocore"
)

func TestInboxListUnpaired(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	_, rpcErr := h.toolInboxList(store.WorkerRec{Name: "piso-worker-demo", Slug: "demo"}, nil)
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "not enrolled") {
		t.Fatalf("expected not-enrolled error, got %v", rpcErr)
	}
}

func TestInboxRejectsWorkerArg(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	h.CoordinatorDo = func(method, path string, query url.Values, body []byte) (int, []byte, error) {
		t.Fatal("coordinator must not run")
		return 0, nil, nil
	}
	caller := store.WorkerRec{PrincipalID: "11111111-1111-1111-1111-111111111111"}
	_, rpcErr := h.toolInboxList(caller, json.RawMessage(`{"worker":"other"}`))
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "rejected") {
		t.Fatalf("%v", rpcErr)
	}
	_, rpcErr = h.toolTaskClaim(caller, json.RawMessage(`{"id":"t1","workerId":"other"}`))
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "rejected") {
		t.Fatalf("%v", rpcErr)
	}
}

func TestInboxListPassesEnvelopePrincipal(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	var gotWorker, gotPath, gotMethod string
	h.CoordinatorDo = func(method, path string, query url.Values, body []byte) (int, []byte, error) {
		gotMethod, gotPath, gotWorker = method, path, query.Get("worker")
		return 200, []byte(`{"tasks":[{"id":"t1","heldByMe":true,"claimable":false,"assignedToMe":true}]}`), nil
	}
	caller := store.WorkerRec{PrincipalID: "prin-1", Name: "piso-worker-demo", Slug: "demo"}
	out, rpcErr := h.toolInboxList(caller, json.RawMessage(`{"status":"open"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/v1/inbox" || gotWorker != "prin-1" {
		t.Fatalf("coord %s %s worker=%s", gotMethod, gotPath, gotWorker)
	}
	doc := out.(map[string]any)
	if doc["status"] != "ok" {
		t.Fatalf("status %v", doc["status"])
	}
	tasks := doc["tasks"].([]any)
	row := tasks[0].(map[string]any)
	if row["id"] != "t1" || row["heldByMe"] != true {
		t.Fatalf("row %v", row)
	}
}

func TestInboxListEmptyOnCoordinator404(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	h.CoordinatorDo = func(method, path string, query url.Values, body []byte) (int, []byte, error) {
		return 404, []byte(`{"error":"not found"}`), nil
	}
	out, rpcErr := h.toolInboxList(store.WorkerRec{PrincipalID: "prin-1"}, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	doc := out.(map[string]any)
	if doc["status"] != "ok" {
		t.Fatalf("%v", doc)
	}
}

func TestInboxCoordinatorUnpairedError(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	h.CoordinatorDo = func(method, path string, query url.Values, body []byte) (int, []byte, error) {
		return 0, nil, gtwc.ErrUnpaired
	}
	out, rpcErr := h.toolInboxList(store.WorkerRec{PrincipalID: "prin-1"}, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if out.(map[string]any)["status"] != "unpaired" {
		t.Fatalf("%v", out)
	}
}

func TestTaskClaimPostsLease(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	var gotPath, gotBody, gotWorker string
	h.CoordinatorDo = func(method, path string, query url.Values, body []byte) (int, []byte, error) {
		gotPath, gotBody, gotWorker = path, string(body), query.Get("worker")
		return 200, []byte(`{"id":"t1","leaseExpiresAt":"2026-01-01T00:00:00Z"}`), nil
	}
	out, rpcErr := h.toolTaskClaim(store.WorkerRec{PrincipalID: "prin-1"}, json.RawMessage(`{"id":"t1","ttl":120}`))
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if gotPath != "/api/v1/tasks/t1/claim" || gotWorker != "prin-1" {
		t.Fatalf("%s worker=%s", gotPath, gotWorker)
	}
	if !strings.Contains(gotBody, `"ttl":120`) {
		t.Fatalf("body %s", gotBody)
	}
	if out.(map[string]any)["id"] != "t1" {
		t.Fatalf("%v", out)
	}
}

func TestTaskClaimConflict(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	h.CoordinatorDo = func(method, path string, query url.Values, body []byte) (int, []byte, error) {
		return 409, []byte(`{"error":"conflict"}`), nil
	}
	_, rpcErr := h.toolTaskClaim(store.WorkerRec{PrincipalID: "prin-1"}, json.RawMessage(`{"id":"t1"}`))
	if rpcErr == nil || rpcErr.Message != "conflict" {
		t.Fatalf("%v", rpcErr)
	}
}

func TestTaskCompleteRequiresSummary(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	tool, ok := h.tools["task_complete"]
	if !ok {
		t.Fatal("task_complete not registered")
	}
	var required []string
	if raw, ok := tool.InputSchema["required"].([]string); ok {
		required = raw
	}
	for _, name := range required {
		if name == "summary" {
			return
		}
	}
	t.Fatalf("summary must be required, got %v", required)
}

func TestDirectivePropose(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	var gotPath, gotBody string
	h.CoordinatorDo = func(method, path string, query url.Values, body []byte) (int, []byte, error) {
		gotPath, gotBody = path, string(body)
		return 201, []byte(`{"id":"d1","status":"pending"}`), nil
	}
	out, rpcErr := h.toolDirectivePropose(store.WorkerRec{PrincipalID: "prin-1"}, json.RawMessage(`{"title":"Host","body":"Add one."}`))
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if gotPath != "/api/v1/directives" || !strings.Contains(gotBody, `"title":"Host"`) {
		t.Fatalf("%s %s", gotPath, gotBody)
	}
	if out.(map[string]any)["status"] != "pending" {
		t.Fatalf("%v", out)
	}
}

func TestToolListIncludesInbox(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	list := h.toolList()
	names := map[string]bool{}
	for _, tdef := range list {
		names[tdef["name"].(string)] = true
	}
	for _, want := range []string{
		"whoami", "mcp_add", "inbox_list", "inbox_get",
		"task_claim", "task_renew", "task_release", "task_complete", "task_fail",
		"task_update", "task_wait",
		"directive_propose",
	} {
		if !names[want] {
			t.Fatalf("missing %s in %#v", want, names)
		}
	}
}

func TestInboxGetAndFail(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	h.CoordinatorDo = func(method, path string, query url.Values, body []byte) (int, []byte, error) {
		if method == http.MethodGet && strings.HasPrefix(path, "/api/v1/inbox/") {
			return 200, []byte(`{"kind":"task","id":"t1","body":"do it"}`), nil
		}
		if strings.HasSuffix(path, "/fail") {
			return 200, []byte(`{"id":"t1","status":"failed"}`), nil
		}
		t.Fatalf("unexpected %s %s", method, path)
		return 0, nil, nil
	}
	got, err := h.toolInboxGet(store.WorkerRec{PrincipalID: "p"}, json.RawMessage(`{"id":"t1"}`))
	if err != nil || got.(map[string]any)["id"] != "t1" {
		t.Fatalf("%v %v", got, err)
	}
	got, err = h.tools["task_fail"].Call(store.WorkerRec{PrincipalID: "p"}, json.RawMessage(`{"id":"t1","requestId":"r1","attemptId":"a1","summary":"blocked","evidence":"no route"}`))
	if err != nil || got.(map[string]any)["status"] != "failed" {
		t.Fatalf("%v %v", got, err)
	}
}
