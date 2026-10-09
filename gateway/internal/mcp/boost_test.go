package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"piso/gateway/internal/aiboost"
	"piso/gateway/internal/store"
)

const remoteBoost = "22222222-2222-2222-2222-222222222222"

// No coordinator principal or pairing is required.
var boostCaller = store.WorkerRec{Name: "piso-worker-demo", Slug: "demo"}

func directBoostHandler(t *testing.T) *Handler {
	t.Helper()
	h := New(nil, nil, t.TempDir())
	h.CoordinatorDo = func(string, string, url.Values, []byte) (int, []byte, error) {
		t.Fatal("boost must never call coordinator")
		return 0, nil, nil
	}
	return h
}
func TestBoostToolsRegistered(t *testing.T) {
	h := directBoostHandler(t)
	for _, name := range []string{"boost_search", "boost_get", "boost_suggest"} {
		if h.tools[name].InputSchema["additionalProperties"] != false {
			t.Fatal(name)
		}
	}
	if !h.tools["boost_suggest"].Write {
		t.Fatal("suggestions must be credential scanned")
	}
}
func TestBoostSearchDirectLibrary(t *testing.T) {
	h := directBoostHandler(t)
	next := "https://untrusted.example/api/library/boosters/?cursor=opaque%3D%3D&q=Go"
	h.BoostList = func(ctx context.Context, q, cursor string) (aiboost.Page, error) {
		if q != "Go" || cursor != "previous==" {
			t.Fatal(q, cursor)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("no deadline")
		}
		return aiboost.Page{Items: []aiboost.Booster{{ID: remoteBoost, Name: "Go"}}, Next: &next}, nil
	}
	out, e := h.toolBoostSearch(boostCaller, json.RawMessage(`{"q":"Go","cursor":"previous=="}`))
	if e != nil {
		t.Fatal(e)
	}
	doc := out.(map[string]any)
	if doc["nextCursor"] != "opaque==" || doc["previousCursor"] != nil || doc["items"].([]aiboost.Booster)[0].ID != remoteBoost {
		t.Fatal(doc)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "untrusted.example") {
		t.Fatal("pagination URL leaked")
	}
	h.BoostList = func(context.Context, string, string) (aiboost.Page, error) { return aiboost.Page{}, nil }
	out, e = h.toolBoostSearch(boostCaller, json.RawMessage(`{}`))
	raw, _ = json.Marshal(out)
	if e != nil || !strings.Contains(string(raw), `"items":[]`) {
		t.Fatal(string(raw), e)
	}
}
func TestBoostGetDirectFullMarkdown(t *testing.T) {
	h := directBoostHandler(t)
	body := "\n" + strings.Repeat("界", 8001) + "\n"
	h.BoostGet = func(ctx context.Context, id string) (aiboost.Booster, error) {
		if id != remoteBoost {
			t.Fatal(id)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("no deadline")
		}
		return aiboost.Booster{ID: id, Name: "Current", Version: "v2", Content: aiboost.Content{Format: "markdown", Body: body}}, nil
	}
	out, e := h.toolBoostGet(boostCaller, json.RawMessage(`{"id":"`+remoteBoost+`"}`))
	if e != nil || out.(aiboost.Booster).Content.Body != body {
		t.Fatal(out, e)
	}
}

const suggestionArgs = `{"name":"Go","description":"Go guidance","content":{"format":"markdown","body":"\n# Go\n\n  keep spacing  \n"},"rationale":"Useful","suggestedTags":["go"],"idempotencyKey":"stable-key"}`

func TestBoostSuggestDirectAndStableKey(t *testing.T) {
	h := directBoostHandler(t)
	calls := 0
	h.BoostSuggest = func(ctx context.Context, d aiboost.Draft, key string) (aiboost.Suggestion, error) {
		calls++
		if key != "stable-key" || d.Name != "Go" || d.Content.Body != "\n# Go\n\n  keep spacing  \n" || d.Description != "Go guidance" || d.Rationale != "Useful" || len(d.SuggestedTags) != 1 {
			t.Fatal(d, key)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("no deadline")
		}
		return aiboost.Suggestion{ID: remoteBoost, Status: "submitted", Revision: 1, Draft: d}, nil
	}
	for i := 0; i < 2; i++ {
		out, e := h.toolBoostSuggest(boostCaller, json.RawMessage(suggestionArgs))
		if e != nil || out.(aiboost.Suggestion).ID != remoteBoost {
			t.Fatal(out, e)
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}
func TestBoostErrorsSanitized(t *testing.T) {
	for _, upstream := range []error{errors.New("sensitive bearer"), aiboost.ErrReconnect} {
		h := directBoostHandler(t)
		h.BoostList = func(context.Context, string, string) (aiboost.Page, error) { return aiboost.Page{}, upstream }
		h.BoostGet = func(context.Context, string) (aiboost.Booster, error) { return aiboost.Booster{}, upstream }
		h.BoostSuggest = func(context.Context, aiboost.Draft, string) (aiboost.Suggestion, error) {
			return aiboost.Suggestion{}, upstream
		}
		for name, args := range map[string]string{"boost_search": `{}`, "boost_get": `{"id":"` + remoteBoost + `"}`, "boost_suggest": suggestionArgs} {
			_, e := h.tools[name].Call(boostCaller, json.RawMessage(args))
			if e == nil || strings.Contains(e.Message, "sensitive") {
				t.Fatal(e)
			}
			if upstream == aiboost.ErrReconnect && !strings.Contains(e.Message, "sign-in") {
				t.Fatal(e)
			}
		}
	}
	h := directBoostHandler(t)
	for name, args := range map[string]string{"boost_search": `{}`, "boost_get": `{"id":"` + remoteBoost + `"}`, "boost_suggest": suggestionArgs} {
		if _, e := h.tools[name].Call(boostCaller, json.RawMessage(args)); e == nil {
			t.Fatal(name)
		}
	}
}
func TestBoostRejectsLegacyAndInvalidInput(t *testing.T) {
	h := directBoostHandler(t)
	h.BoostList = func(context.Context, string, string) (aiboost.Page, error) {
		t.Fatal("invalid input reached upstream")
		return aiboost.Page{}, nil
	}
	h.BoostGet = func(context.Context, string) (aiboost.Booster, error) {
		t.Fatal("invalid input reached upstream")
		return aiboost.Booster{}, nil
	}
	h.BoostSuggest = func(context.Context, aiboost.Draft, string) (aiboost.Suggestion, error) {
		t.Fatal("invalid input reached upstream")
		return aiboost.Suggestion{}, nil
	}
	cases := map[string][]string{
		"boost_search":  {`{"worker":"other"}`, `{"limit":25}`, `{"offset":0}`, `{"q":true}`, `[]`, `null`, `{} {}`, `{"q":"` + strings.Repeat("x", 501) + `"}`},
		"boost_get":     {`{"id":"../x"}`, `{"id":"` + remoteBoost + `","worker":"other"}`},
		"boost_suggest": {`{"title":"old","body":"old"}`, strings.Replace(suggestionArgs, `"stable-key"`, `""`, 1), strings.Replace(suggestionArgs, `"markdown"`, `"html"`, 1), strings.Replace(suggestionArgs, `"Useful"`, `""`, 1), strings.Replace(suggestionArgs, `"body":`, `"owner":"spoof","body":`, 1), strings.Replace(suggestionArgs, `"stable-key"`, `"x\r\ny"`, 1)},
	}
	for name, args := range cases {
		for _, arg := range args {
			if _, e := h.tools[name].Call(boostCaller, json.RawMessage(arg)); e == nil || e.Code != codeInvalidParams {
				t.Fatalf("accepted %s: %s (%v)", name, arg, e)
			}
		}
	}
}
