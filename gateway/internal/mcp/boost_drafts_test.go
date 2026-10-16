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

func TestDraftTools(t *testing.T) {
	h := directBoostHandler(t)
	for _, name := range []string{"boost_list_drafts", "boost_get_draft", "boost_draft_version"} {
		tool, ok := h.tools[name]
		if !ok || tool.InputSchema["additionalProperties"] != false || tool.Write != (name == "boost_draft_version") {
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
	out := aiboost.Draft{ID: remoteBoost, Type: "new_version", Version: "1.1.0", Revision: 1}
	h.BoostListDrafts = func(ctx context.Context, status, cursor string) (aiboost.DraftPage, error) {
		checkContext(ctx)
		if status != "submitted" || cursor != "opaque==" {
			t.Fatal(status, cursor)
		}
		return aiboost.DraftPage{Items: []aiboost.Draft{out}, NextCursor: "next=="}, nil
	}
	h.BoostGetDraft = func(ctx context.Context, id string) (aiboost.Draft, error) {
		checkContext(ctx)
		if id != remoteBoost {
			t.Fatal(id)
		}
		return out, nil
	}
	h.BoostDraftVersion = func(ctx context.Context, d aiboost.VersionDraft, key string) (aiboost.Draft, error) {
		checkContext(ctx)
		if d.BoosterID != remoteBoost || d.Version != "1.1.0" || d.Content.Body != "\n# Updated\n" || key != "stable-version" {
			t.Fatal(d, key)
		}
		return out, nil
	}
	for name, args := range map[string]string{
		"boost_list_drafts":   `{"status":"submitted","cursor":"opaque=="}`,
		"boost_get_draft":     `{"id":"` + remoteBoost + `"}`,
		"boost_draft_version": versionArgs,
	} {
		got, err := h.tools[name].Call(boostCaller, json.RawMessage(args))
		raw, _ := json.Marshal(got)
		if err != nil || !strings.Contains(string(raw), "new_version") {
			t.Fatal(name, got, err)
		}
	}
	h.BoostListDrafts = func(context.Context, string, string) (aiboost.DraftPage, error) {
		return aiboost.DraftPage{}, nil
	}
	got, err := h.toolBoostListDrafts(boostCaller, json.RawMessage(`{}`))
	raw, _ := json.Marshal(got)
	if err != nil || !strings.Contains(string(raw), `"items":[]`) {
		t.Fatal(got, err)
	}
}

func TestDraftToolsInvalidInput(t *testing.T) {
	h := directBoostHandler(t)
	cases := map[string][]string{
		"boost_list_drafts":   {`{"status":"pending"}`, `{"type":"new_version"}`, `{"worker":"other"}`, `{"cursor":"` + strings.Repeat("x", 4097) + `"}`, `[]`, `null`, `{} {}`},
		"boost_get_draft":     {`{}`, `{"id":"../approve"}`, `{"id":"` + remoteBoost + `","url":"https://evil"}`},
		"boost_draft_version": {`{}`, strings.Replace(versionArgs, `"1.1.0"`, `"v1.1.0"`, 1), strings.Replace(versionArgs, `"Checks"`, `" "`, 1), strings.Replace(versionArgs, `"stable-version"`, `"bad\nkey"`, 1), strings.Replace(versionArgs, `"boosterId":`, `"owner":"spoof","boosterId":`, 1), strings.Replace(versionArgs, `"body":`, `"owner":"spoof","body":`, 1)},
		"boost_edit_draft":    {`{"id":"` + remoteBoost + `","expectedRevision":1,"version":null}`, `{"id":"` + remoteBoost + `","expectedRevision":1,"version":"1.1.0","name":"mixed"}`},
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

func TestDraftToolsFailuresAndScanning(t *testing.T) {
	h := directBoostHandler(t)
	calls := map[string]string{"boost_list_drafts": `{}`, "boost_get_draft": `{"id":"` + remoteBoost + `"}`, "boost_draft_version": versionArgs}
	for name, args := range calls {
		if _, err := h.tools[name].Call(boostCaller, json.RawMessage(args)); err == nil {
			t.Fatal("missing connection accepted", name)
		}
	}
	for _, upstream := range []error{errors.New("sensitive credential"), aiboost.ErrReconnect, &aiboost.APIError{Status: 409}, &aiboost.APIError{Status: 403}} {
		h.BoostListDrafts = func(context.Context, string, string) (aiboost.DraftPage, error) {
			return aiboost.DraftPage{}, upstream
		}
		h.BoostGetDraft = func(context.Context, string) (aiboost.Draft, error) { return aiboost.Draft{}, upstream }
		h.BoostDraftVersion = func(context.Context, aiboost.VersionDraft, string) (aiboost.Draft, error) {
			return aiboost.Draft{}, upstream
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
	h.BoostDraftVersion = func(context.Context, aiboost.VersionDraft, string) (aiboost.Draft, error) {
		t.Fatal("credential reached provider")
		return aiboost.Draft{}, nil
	}
	args := strings.Replace(versionArgs, "Useful", "sk-"+strings.Repeat("x", 30), 1)
	params, _ := json.Marshal(map[string]any{"name": "boost_draft_version", "arguments": json.RawMessage(args)})
	response := h.callTool(boostCaller, rpcRequest{Params: params})
	raw, _ := json.Marshal(response)
	if !strings.Contains(string(raw), "credential pattern blocked") || strings.Contains(string(raw), "sk-") {
		t.Fatal(string(raw))
	}
}

func TestVersionDraftEditForwarding(t *testing.T) {
	h := directBoostHandler(t)
	h.BoostEditDraft = func(ctx context.Context, id string, e aiboost.DraftEdit) (aiboost.Draft, error) {
		if id != remoteBoost || e.ExpectedRevision != 2 || e.Version == nil || *e.Version != "1.2.0" || e.BaseVersionID == nil || *e.BaseVersionID != remoteBoost || e.Changelog == nil || *e.Changelog != "Rebased" || e.Name != nil {
			t.Fatal(e)
		}
		return aiboost.Draft{ID: id, Revision: 3, Type: "new_version"}, nil
	}
	_, err := h.toolBoostEditDraft(boostCaller, json.RawMessage(`{"id":"`+remoteBoost+`","expectedRevision":2,"baseVersionId":"`+remoteBoost+`","version":"1.2.0","changelog":"Rebased"}`))
	if err != nil {
		t.Fatal(err)
	}
}
