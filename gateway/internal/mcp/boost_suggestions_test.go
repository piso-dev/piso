package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"piso/gateway/internal/aiboost"
	"piso/gateway/internal/patterns"
)

const versionArgs = `{"boosterId":"22222222-2222-2222-2222-222222222222","baseVersionId":"11111111-1111-1111-1111-111111111111","version":"1.1.0","content":{"format":"markdown","body":"\n# Updated\n"},"changelog":"Checks","rationale":"Useful","idempotencyKey":"stable-version"}`

func TestSuggestionTools(t *testing.T) {
	h := directBoostHandler(t)
	for _, name := range []string{"boost_list_suggestions", "boost_get_suggestion", "boost_suggest_version"} {
		tool, ok := h.tools[name]
		if !ok || tool.InputSchema["additionalProperties"] != false || tool.Write != (name == "boost_suggest_version") {
			t.Fatal(name)
		}
	}
	for name := range h.tools {
		if strings.HasPrefix(name, "boost_") && (strings.Contains(name, "approve") || strings.Contains(name, "reject") || strings.Contains(name, "publish")) {
			t.Fatal(name)
		}
	}
	checkContext := func(ctx context.Context) {
		t.Helper()
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing deadline")
		}
	}
	out := aiboost.Suggestion{ID: remoteBoost, Type: "new_version", Version: "1.1.0", Revision: 1}
	h.BoostListSuggestions = func(ctx context.Context, status, cursor string) (aiboost.SuggestionPage, error) {
		checkContext(ctx)
		if status != "submitted" || cursor != "opaque==" {
			t.Fatal(status, cursor)
		}
		return aiboost.SuggestionPage{Items: []aiboost.Suggestion{out}, NextCursor: "next=="}, nil
	}
	h.BoostGetSuggestion = func(ctx context.Context, id string) (aiboost.Suggestion, error) {
		checkContext(ctx)
		if id != remoteBoost {
			t.Fatal(id)
		}
		return out, nil
	}
	h.BoostSuggestVersion = func(ctx context.Context, d aiboost.VersionDraft, key string) (aiboost.Suggestion, error) {
		checkContext(ctx)
		if d.BoosterID != remoteBoost || d.Version != "1.1.0" || d.Content.Body != "\n# Updated\n" || key != "stable-version" {
			t.Fatal(d, key)
		}
		return out, nil
	}
	for name, args := range map[string]string{
		"boost_list_suggestions": `{"status":"submitted","cursor":"opaque=="}`,
		"boost_get_suggestion":   `{"id":"` + remoteBoost + `"}`,
		"boost_suggest_version":  versionArgs,
	} {
		got, err := h.tools[name].Call(boostCaller, json.RawMessage(args))
		raw, _ := json.Marshal(got)
		if err != nil || !strings.Contains(string(raw), "new_version") {
			t.Fatal(name, got, err)
		}
	}
	h.BoostListSuggestions = func(context.Context, string, string) (aiboost.SuggestionPage, error) {
		return aiboost.SuggestionPage{}, nil
	}
	got, err := h.toolBoostListSuggestions(boostCaller, json.RawMessage(`{}`))
	raw, _ := json.Marshal(got)
	if err != nil || !strings.Contains(string(raw), `"items":[]`) {
		t.Fatal(got, err)
	}
}

func TestSuggestionToolsInvalidInput(t *testing.T) {
	h := directBoostHandler(t)
	cases := map[string][]string{
		"boost_list_suggestions": {`{"status":"pending"}`, `{"type":"new_version"}`, `{"worker":"other"}`, `{"cursor":"` + strings.Repeat("x", 4097) + `"}`, `[]`, `null`, `{} {}`},
		"boost_get_suggestion":   {`{}`, `{"id":"../approve"}`, `{"id":"` + remoteBoost + `","url":"https://evil"}`},
		"boost_suggest_version":  {`{}`, strings.Replace(versionArgs, `"1.1.0"`, `"v1.1.0"`, 1), strings.Replace(versionArgs, `"Checks"`, `" "`, 1), strings.Replace(versionArgs, `"stable-version"`, `"bad\nkey"`, 1), strings.Replace(versionArgs, `"boosterId":`, `"owner":"spoof","boosterId":`, 1), strings.Replace(versionArgs, `"body":`, `"owner":"spoof","body":`, 1)},
		"boost_edit_suggestion":  {`{"id":"` + remoteBoost + `","expectedRevision":1,"version":null}`, `{"id":"` + remoteBoost + `","expectedRevision":1,"version":"1.1.0","name":"mixed"}`},
	}
	for name, inputs := range cases {
		for _, args := range inputs {
			_, err := h.tools[name].Call(boostCaller, json.RawMessage(args))
			if err == nil || err.Code != codeInvalidParams {
				t.Fatal(name, args, err)
			}
		}
	}
}

func TestSuggestionToolsFailuresAndScanning(t *testing.T) {
	h := directBoostHandler(t)
	calls := map[string]string{"boost_list_suggestions": `{}`, "boost_get_suggestion": `{"id":"` + remoteBoost + `"}`, "boost_suggest_version": versionArgs}
	for name, args := range calls {
		if _, err := h.tools[name].Call(boostCaller, json.RawMessage(args)); err == nil {
			t.Fatal("missing connection accepted", name)
		}
	}
	for _, upstream := range []error{errors.New("sensitive credential"), aiboost.ErrReconnect, &aiboost.APIError{Status: 409}, &aiboost.APIError{Status: 403}} {
		h.BoostListSuggestions = func(context.Context, string, string) (aiboost.SuggestionPage, error) {
			return aiboost.SuggestionPage{}, upstream
		}
		h.BoostGetSuggestion = func(context.Context, string) (aiboost.Suggestion, error) { return aiboost.Suggestion{}, upstream }
		h.BoostSuggestVersion = func(context.Context, aiboost.VersionDraft, string) (aiboost.Suggestion, error) {
			return aiboost.Suggestion{}, upstream
		}
		for name, args := range calls {
			_, err := h.tools[name].Call(boostCaller, json.RawMessage(args))
			if err == nil || strings.Contains(err.Message, "sensitive") {
				t.Fatal(name, err)
			}
		}
	}
	var err error
	h.Patterns, err = patterns.Load("")
	if err != nil {
		t.Fatal(err)
	}
	h.BoostSuggestVersion = func(context.Context, aiboost.VersionDraft, string) (aiboost.Suggestion, error) {
		t.Fatal("credential reached provider")
		return aiboost.Suggestion{}, nil
	}
	args := strings.Replace(versionArgs, "Useful", "sk-"+strings.Repeat("x", 30), 1)
	params, _ := json.Marshal(map[string]any{"name": "boost_suggest_version", "arguments": json.RawMessage(args)})
	response := h.callTool(boostCaller, rpcRequest{Params: params})
	raw, _ := json.Marshal(response)
	if !strings.Contains(string(raw), "credential pattern blocked") || strings.Contains(string(raw), "sk-") {
		t.Fatal(string(raw))
	}
}

func TestVersionSuggestionEditForwarding(t *testing.T) {
	h := directBoostHandler(t)
	h.BoostEditSuggestion = func(ctx context.Context, id string, e aiboost.SuggestionEdit) (aiboost.Suggestion, error) {
		if id != remoteBoost || e.ExpectedRevision != 2 || e.Version == nil || *e.Version != "1.2.0" || e.BaseVersionID == nil || *e.BaseVersionID != remoteBoost || e.Changelog == nil || *e.Changelog != "Rebased" || e.Name != nil {
			t.Fatal(e)
		}
		return aiboost.Suggestion{ID: id, Revision: 3, Type: "new_version"}, nil
	}
	_, err := h.toolBoostEditSuggestion(boostCaller, json.RawMessage(`{"id":"`+remoteBoost+`","expectedRevision":2,"baseVersionId":"`+remoteBoost+`","version":"1.2.0","changelog":"Rebased"}`))
	if err != nil {
		t.Fatal(err)
	}
}
