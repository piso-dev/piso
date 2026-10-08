package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"piso/gateway/internal/aiboost"
	"piso/gateway/internal/store"
)

const localBoost = "11111111-1111-1111-1111-111111111111"
const remoteBoost = "22222222-2222-2222-2222-222222222222"

var boostCaller = store.WorkerRec{PrincipalID: "envelope-principal", Name: "piso-worker-demo", Slug: "demo"}

func TestBoostToolsRegistered(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	for _, name := range []string{"boost_search", "boost_get", "boost_suggest"} {
		tool, ok := h.tools[name]
		if !ok || tool.InputSchema["additionalProperties"] != false {
			t.Fatal(name)
		}
	}
	if !h.tools["boost_suggest"].Write {
		t.Fatal("suggestions must be credential scanned")
	}
}

func TestBoostSearchScopedCatalog(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	h.CoordinatorDo = func(method, path string, q url.Values, body []byte) (int, []byte, error) {
		if method != "GET" || path != "/api/v1/boosters" || q.Get("worker") != boostCaller.PrincipalID {
			t.Fatalf("%s %s %v", method, path, q)
		}
		return 200, []byte(`{"boosters":[{"id":"b","title":"Go testing","aiBoostId":"hidden","excerpt":"hidden"},{"id":"a","title":"Go design"},{"id":"c","title":"Python"}]}`), nil
	}
	h.BoostGet = func(context.Context, string) (aiboost.Booster, error) {
		t.Fatal("search must not fetch account-wide content")
		return aiboost.Booster{}, nil
	}
	out, e := h.toolBoostSearch(boostCaller, json.RawMessage(`{"q":"go","limit":1}`))
	if e != nil {
		t.Fatal(e)
	}
	doc := out.(map[string]any)
	items := doc["boosters"].([]map[string]any)
	if len(items) != 1 || items[0]["id"] != "a" || doc["nextOffset"] != 1 {
		t.Fatal(doc)
	}
	out, e = h.toolBoostSearch(boostCaller, json.RawMessage(`{"q":"go","limit":1,"offset":1}`))
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "hidden") || out.(map[string]any)["nextOffset"] != nil {
		t.Fatal(string(raw))
	}
}

func TestBoostGetAuthorizesAndCaps(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	authorized := false
	h.CoordinatorDo = func(method, path string, q url.Values, body []byte) (int, []byte, error) {
		if method != "GET" || path != "/api/v1/boosters/"+localBoost || q.Get("worker") != boostCaller.PrincipalID {
			t.Fatalf("%s %s %v", method, path, q)
		}
		authorized = true
		return 200, []byte(`{"id":"` + localBoost + `","aiBoostId":"` + remoteBoost + `","title":"old","excerpt":"stale","secret":"hidden"}`), nil
	}
	h.BoostGet = func(ctx context.Context, id string) (aiboost.Booster, error) {
		if !authorized || id != remoteBoost {
			t.Fatal("unauthorized fetch", id)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("no deadline")
		}
		return aiboost.Booster{ID: id, Name: "Current", Version: "v2", Content: aiboost.Content{Format: "markdown", Body: strings.Repeat("界", 8001)}}, nil
	}
	out, e := h.toolBoostGet(boostCaller, json.RawMessage(`{"id":"`+localBoost+`"}`))
	if e != nil {
		t.Fatal(e)
	}
	doc := out.(map[string]any)
	if doc["id"] != localBoost || doc["title"] != "Current" || len([]rune(doc["excerpt"].(string))) != 8000 || doc["truncated"] != true || len(doc["contentHash"].(string)) != 64 {
		t.Fatal("invalid excerpt")
	}
	raw, _ := json.Marshal(doc)
	if strings.Contains(string(raw), "hidden") || strings.Contains(string(raw), "stale") {
		t.Fatal("unfiltered metadata")
	}
	// Provider errors must never become agent-visible text.
	h.BoostGet = func(context.Context, string) (aiboost.Booster, error) {
		return aiboost.Booster{}, errors.New("sensitive bearer")
	}
	_, e = h.toolBoostGet(boostCaller, json.RawMessage(`{"id":"`+localBoost+`"}`))
	if e == nil || strings.Contains(e.Message, "sensitive") {
		t.Fatal(e)
	}
}

func TestBoostFailsClosed(t *testing.T) {
	for _, status := range []int{0, 302, 403, 404, 500, 200} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			h := New(nil, nil, t.TempDir())
			h.BoostGet = func(context.Context, string) (aiboost.Booster, error) {
				t.Fatal("must not fetch")
				return aiboost.Booster{}, nil
			}
			if status != 0 {
				h.CoordinatorDo = func(string, string, url.Values, []byte) (int, []byte, error) { return status, []byte(`{}`), nil }
			}
			_, e := h.toolBoostGet(boostCaller, json.RawMessage(`{"id":"`+localBoost+`"}`))
			if e == nil {
				t.Fatal("must deny")
			}
		})
	}
}

func TestBoostRejectsSpoofingAndBadInput(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	h.CoordinatorDo = func(string, string, url.Values, []byte) (int, []byte, error) {
		t.Fatal("invalid args reached coordinator")
		return 0, nil, nil
	}
	cases := map[string][]string{
		"boost_search":  {`{"worker":"other"}`, `{"workerId":"other"}`, `{"limit":0}`, `{"offset":-1}`, `{"q":true}`, `[]`, `null`},
		"boost_get":     {`{"id":"../x"}`, `{"id":"` + localBoost + `","aiBoostId":"other"}`, `{"id":"` + localBoost + `","worker":"other"}`},
		"boost_suggest": {`{"title":"t","body":"b","worker":"other"}`, `{"title":"t"}`, `{"title":"t","body":"b","publish":true}`},
	}
	for name, args := range cases {
		for _, arg := range args {
			_, e := h.tools[name].Call(boostCaller, json.RawMessage(arg))
			if e == nil {
				t.Fatalf("accepted %s %s", name, arg)
			}
		}
	}
}

func TestBoostSuggestUsesApprovalPath(t *testing.T) {
	h := New(nil, nil, t.TempDir())
	h.CoordinatorDo = func(method, path string, q url.Values, body []byte) (int, []byte, error) {
		if method != "POST" || path != "/api/v1/boosters/proposals" || q.Get("worker") != boostCaller.PrincipalID {
			t.Fatalf("%s %s %v", method, path, q)
		}
		var payload map[string]any
		json.Unmarshal(body, &payload)
		if payload["title"] != "New guidance" || payload["body"] != "# Markdown\n\nKeep spacing" || payload["booster"] != localBoost {
			t.Fatal(payload)
		}
		return 201, []byte(`{"id":"proposal","approvalId":"approval","status":"pending"}`), nil
	}
	out, e := h.toolBoostSuggest(boostCaller, json.RawMessage(`{"title":"New guidance","body":"# Markdown\n\nKeep spacing","booster":"`+localBoost+`"}`))
	if e != nil || out.(map[string]any)["approvalId"] != "approval" {
		t.Fatal(out, e)
	}
	h.CoordinatorDo = func(string, string, url.Values, []byte) (int, []byte, error) {
		return 404, []byte(`{"error":"not found"}`), nil
	}
	_, e = h.toolBoostSuggest(boostCaller, json.RawMessage(`{"title":"t","body":"b"}`))
	if e == nil {
		t.Fatal("suggest permission bypass")
	}
}
