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

const editArgs = `{"id":"` + remoteBoost + `","expectedRevision":1,"name":"Updated"}`

func TestBoostEditDraft(t *testing.T) {
	h := directBoostHandler(t)
	tool, ok := h.tools["boost_edit_draft"]
	if !ok || !tool.Write || tool.InputSchema["additionalProperties"] != false {
		t.Fatal("missing strict write tool")
	}
	for _, name := range []string{"boost_approve", "boost_reject", "boost_approve_draft", "boost_reject_draft"} {
		if _, ok := h.tools[name]; ok {
			t.Fatal("worker decision exposed", name)
		}
	}
	calls := 0
	h.BoostEditDraft = func(ctx context.Context, id string, e aiboost.DraftEdit) (aiboost.Draft, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("no deadline")
		}
		if id != remoteBoost || e.ExpectedRevision != 1 || e.Name == nil || *e.Name != "Updated" || e.Content != nil || e.Description != nil || e.Rationale != nil || e.SuggestedTags != nil {
			t.Fatal(id, e)
		}
		return aiboost.Draft{ID: id, Revision: 2, Status: "submitted"}, nil
	}
	out, err := tool.Call(boostCaller, json.RawMessage(editArgs))
	if err != nil || out.(aiboost.Draft).Revision != 2 || calls != 1 {
		t.Fatal(out, err, calls)
	}
}

func TestBoostEditRejectsInvalidInput(t *testing.T) {
	h := directBoostHandler(t)
	h.BoostEditDraft = func(context.Context, string, aiboost.DraftEdit) (aiboost.Draft, error) {
		t.Fatal("invalid input reached provider")
		return aiboost.Draft{}, nil
	}
	for _, args := range []string{
		`null`, `[]`, `{}`, editArgs + ` {}`,
		strings.Replace(editArgs, remoteBoost, "../approve", 1),
		strings.Replace(editArgs, `"expectedRevision":1,`, "", 1),
		strings.Replace(editArgs, `"expectedRevision":1`, `"expectedRevision":0`, 1),
		strings.Replace(editArgs, `"expectedRevision":1`, `"expectedRevision":1.5`, 1),
		strings.Replace(editArgs, `"name":"Updated"`, `"changeSummary":"only note"`, 1),
		strings.Replace(editArgs, `"name":"Updated"`, `"name":null`, 1),
		strings.Replace(editArgs, `"name":"Updated"`, `"name":" "`, 1),
		strings.Replace(editArgs, `"name":"Updated"`, `"suggestedTags":null,"name":"valid"`, 1),
		strings.Replace(editArgs, `"name":"Updated"`, `"content":{"format":"markdown","body":"x","owner":"spoof"}`, 1),
		strings.Replace(editArgs, `"name":"Updated"`, `"content":{"body":"x"}`, 1),
		strings.Replace(editArgs, `"name":"Updated"`, `"content":{"format":"markdown","body":null}`, 1),
		strings.Replace(editArgs, `"name":"Updated"`, `"suggestedTags":[null]`, 1),
	} {
		if _, err := h.toolBoostEditDraft(boostCaller, json.RawMessage(args)); err == nil || err.Code != codeInvalidParams {
			t.Fatal("accepted", args, err)
		}
	}
	for _, field := range []string{"owner", "destination", "status", "decision", "worker", "idempotencyKey", "url"} {
		args := strings.TrimSuffix(editArgs, "}") + `,"` + field + `":"spoof"}`
		if _, err := h.toolBoostEditDraft(boostCaller, json.RawMessage(args)); err == nil || err.Code != codeInvalidParams {
			t.Fatal("accepted", field, err)
		}
	}
}

func TestBoostEditErrors(t *testing.T) {
	h := directBoostHandler(t)
	if _, err := h.toolBoostEditDraft(boostCaller, json.RawMessage(editArgs)); err == nil {
		t.Fatal("accepted unavailable")
	}
	for _, upstream := range []error{errors.New("sensitive credential"), aiboost.ErrReconnect, &aiboost.APIError{Status: 400}, &aiboost.APIError{Status: 401}, &aiboost.APIError{Status: 403}, &aiboost.APIError{Status: 404}, &aiboost.APIError{Status: 409}, &aiboost.APIError{Status: 429}, &aiboost.APIError{Status: 500}} {
		calls := 0
		h.BoostEditDraft = func(context.Context, string, aiboost.DraftEdit) (aiboost.Draft, error) {
			calls++
			return aiboost.Draft{}, upstream
		}
		_, err := h.toolBoostEditDraft(boostCaller, json.RawMessage(editArgs))
		if err == nil || calls != 1 || strings.Contains(err.Message, "sensitive") || strings.Contains(err.Message, "idempotency") || strings.Contains(err.Message, "submission key") {
			t.Fatal(err, calls)
		}
		if api, ok := upstream.(*aiboost.APIError); ok && api.Status == 409 && !strings.Contains(err.Message, "do not guess") {
			t.Fatal(err)
		}
	}
}

func TestBoostEditCredentialScanning(t *testing.T) {
	h := directBoostHandler(t)
	var err error
	h.Patterns, err = patterns.Load("")
	if err != nil {
		t.Fatal(err)
	}
	h.BoostEditDraft = func(context.Context, string, aiboost.DraftEdit) (aiboost.Draft, error) {
		t.Fatal("credential reached provider")
		return aiboost.Draft{}, nil
	}
	args := strings.Replace(editArgs, "Updated", "sk-"+strings.Repeat("x", 30), 1)
	params, _ := json.Marshal(map[string]any{"name": "boost_edit_draft", "arguments": json.RawMessage(args)})
	response := h.callTool(boostCaller, rpcRequest{Params: params})
	raw, _ := json.Marshal(response)
	if !strings.Contains(string(raw), "credential pattern blocked") || strings.Contains(string(raw), "sk-") {
		t.Fatal(string(raw))
	}
}
