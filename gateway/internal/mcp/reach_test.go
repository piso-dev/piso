package mcp

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

func TestCapabilitiesFromEnvelope(t *testing.T) {
	st := activityStore(t)
	if err := st.AddSecret(store.SecretRec{ID: "s1", EnvKey: "GITHUB_TOKEN", Placeholder: "piso_github", Workers: []string{"demo"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{ID: "s2", EnvKey: "OTHER", Placeholder: "piso_other", Workers: []string{"other"}}); err != nil {
		t.Fatal(err)
	}
	h := New(st, nil, t.TempDir())
	out, err := h.toolCapabilities(store.WorkerRec{Name: "piso-worker-demo", Slug: "demo", InternetDisabled: true}, nil)
	if err != nil {
		t.Fatal(err.Message)
	}
	doc := out.(map[string]any)
	if doc["internetEnabled"] != false || doc["slug"] != "demo" {
		t.Fatalf("%v", doc)
	}
	scoped := doc["scopedPlaceholders"].([]string)
	if len(scoped) != 1 || scoped[0] != "piso_github" {
		t.Fatalf("%v", scoped)
	}
	_, err = h.toolCapabilities(store.WorkerRec{Name: "piso-worker-demo", Slug: "demo"}, json.RawMessage(`{"worker":"other"}`))
	if err == nil || !strings.Contains(err.Message, "rejected") {
		t.Fatalf("%v", err)
	}
}

func TestIngressRequestStatusCancel(t *testing.T) {
	st := activityStore(t)
	h := New(st, nil, t.TempDir())
	caller := store.WorkerRec{Name: "piso-worker-demo", Slug: "demo"}
	out, rpcErr := h.toolIngressRequest(caller, json.RawMessage(`{"port":19432,"note":"plan ui"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	doc := out.(map[string]any)
	if doc["status"] != store.IngressStatusPending || doc["name"] != "plan-demo" || doc["created"] != true {
		t.Fatalf("%v", doc)
	}
	st2, rpcErr := h.toolIngressStatus(caller, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if st2.(map[string]any)["id"] != doc["id"] {
		t.Fatalf("%v", st2)
	}
	_, rpcErr = h.toolIngressCancel(caller, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	none, rpcErr := h.toolIngressStatus(caller, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if none.(map[string]any)["status"] != "none" {
		t.Fatalf("%v", none)
	}
}

func TestRoutesOwnWorkerOnly(t *testing.T) {
	st := activityStore(t)
	if err := st.AddRoute(store.RouteRec{ID: "r1", Name: "demo-8080", Worker: "piso-worker-demo", Port: 8080, Origin: store.AutoRouteOriginAuto}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRoute(store.RouteRec{ID: "r2", Name: "other-9", Worker: "piso-worker-other", Port: 9, Origin: store.AutoRouteOriginExpose}); err != nil {
		t.Fatal(err)
	}
	h := New(st, nil, t.TempDir())
	out, rpcErr := h.toolRoutes(store.WorkerRec{Name: "piso-worker-demo", Slug: "demo"}, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	routes := out.(map[string]any)["routes"].([]map[string]any)
	if len(routes) != 1 || routes[0]["name"] != "demo-8080" {
		t.Fatalf("%v", routes)
	}
}

func TestAsksUnpairedAndCoordinator(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	_, rpcErr := h.toolApprovalRequest(store.WorkerRec{Name: "piso-worker-demo", Slug: "demo"}, json.RawMessage(`{"action":"publish","title":"Ship"}`))
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "not enrolled") {
		t.Fatalf("expected not-enrolled error, got %v", rpcErr)
	}
	h = New(nil, nil, t.TempDir())
	var gotPath, gotWorker, gotBody string
	h.CoordinatorDo = func(method, path string, query url.Values, body []byte) (int, []byte, error) {
		gotPath, gotWorker, gotBody = path, query.Get("worker"), string(body)
		if method == http.MethodPost && path == "/api/v1/approvals" {
			return 201, []byte(`{"id":"a1","status":"pending"}`), nil
		}
		if method == http.MethodGet && path == "/api/v1/approvals" {
			return 200, []byte(`{"status":"ok","approvals":[{"id":"a1","status":"approved"}]}`), nil
		}
		if method == http.MethodGet && path == "/api/v1/approvals/a1" {
			return 200, []byte(`{"id":"a1","status":"approved"}`), nil
		}
		if method == http.MethodPost && path == "/api/v1/human-actions" {
			return 201, []byte(`{"id":"h1","status":"requested"}`), nil
		}
		if method == http.MethodGet && path == "/api/v1/human-actions" {
			return 200, []byte(`{"status":"ok","humanActions":[{"id":"h1","status":"completed"}]}`), nil
		}
		if path == "/api/v1/human-actions/h1" {
			return 200, []byte(`{"id":"h1","status":"completed","evidence":"ok"}`), nil
		}
		t.Fatalf("%s %s", method, path)
		return 0, nil, nil
	}
	caller := store.WorkerRec{PrincipalID: "prin-1", Name: "piso-worker-demo", Slug: "demo"}
	created, rpcErr := h.toolApprovalRequest(caller, json.RawMessage(`{"action":"publish","title":"Ship"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if gotPath != "/api/v1/approvals" || gotWorker != "prin-1" || !strings.Contains(gotBody, `"title":"Ship"`) {
		t.Fatalf("%s %s %s", gotPath, gotWorker, gotBody)
	}
	if created.(map[string]any)["status"] != "pending" {
		t.Fatalf("%v", created)
	}
	st, rpcErr := h.toolApprovalStatus(caller, json.RawMessage(`{"id":"a1"}`))
	if rpcErr != nil || st.(map[string]any)["status"] != "approved" {
		t.Fatalf("%v %v", st, rpcErr)
	}
	ha, rpcErr := h.toolHumanActionRequest(caller, json.RawMessage(`{"category":"dns","title":"Add host","instructions":"Pair it."}`))
	if rpcErr != nil || ha.(map[string]any)["status"] != "requested" {
		t.Fatalf("%v %v", ha, rpcErr)
	}
	hs, rpcErr := h.toolHumanActionStatus(caller, json.RawMessage(`{"id":"h1"}`))
	if rpcErr != nil || hs.(map[string]any)["status"] != "completed" {
		t.Fatalf("%v %v", hs, rpcErr)
	}
	listed, rpcErr := h.toolApprovalList(caller, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if listed.(map[string]any)["status"] != "ok" {
		t.Fatalf("%v", listed)
	}
	hl, rpcErr := h.toolHumanActionList(caller, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr.Message)
	}
	if hl.(map[string]any)["status"] != "ok" {
		t.Fatalf("%v", hl)
	}
	_, rpcErr = h.toolApprovalRequest(caller, json.RawMessage(`{"action":"x","title":"y","worker":"nope"}`))
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "rejected") {
		t.Fatalf("%v", rpcErr)
	}
}

func TestToolListIncludesReach(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	names := map[string]bool{}
	for _, tdef := range h.toolList() {
		names[tdef["name"].(string)] = true
	}
	for _, want := range []string{
		"capabilities", "ingress_request", "ingress_status", "ingress_cancel", "routes",
		"approval_request", "approval_list", "approval_status",
		"human_action_request", "human_action_list", "human_action_status",
	} {
		if !names[want] {
			t.Fatalf("missing %s", want)
		}
	}
}
