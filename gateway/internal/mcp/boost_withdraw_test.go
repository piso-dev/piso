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

func TestWithdrawDraftToolRegistered(t *testing.T) {
	h := directBoostHandler(t)
	tool, ok := h.tools["boost_withdraw_draft"]
	if !ok {
		t.Fatal("boost_withdraw_draft must be registered")
	}
	if tool.InputSchema["additionalProperties"] != false || !tool.Write {
		t.Fatal("withdrawal must be strict and credential scanned")
	}
	if tool.Call == nil {
		t.Fatal("missing call")
	}
	// Approve/reject/publish stay host-only; withdrawal is author authority.
	for name := range h.tools {
		if strings.Contains(name, "approve") || strings.Contains(name, "reject") || strings.Contains(name, "publish") {
			t.Fatal(name)
		}
	}
}

func TestWithdrawDraftForwardsBothForms(t *testing.T) {
	h := directBoostHandler(t)
	seen := []aiboost.Withdrawal{}
	h.BoostWithdraw = func(ctx context.Context, w aiboost.Withdrawal) (aiboost.Draft, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing deadline")
		}
		seen = append(seen, w)
		return aiboost.Draft{ID: remoteBoost, Status: "withdrawn", Revision: 1,
			Withdrawal: &aiboost.DraftWithdrawal{Revision: 1, Reason: "oops"}}, nil
	}
	for _, args := range []string{
		`{"id":"` + remoteBoost + `","expectedRevision":3,"reason":"oops"}`,
		`{"idempotencyKey":"misfire-key"}`,
	} {
		got, err := h.tools["boost_withdraw_draft"].Call(boostCaller, json.RawMessage(args))
		raw, _ := json.Marshal(got)
		if err != nil || !strings.Contains(string(raw), `"withdrawn"`) || !strings.Contains(string(raw), `"withdrawal"`) {
			t.Fatal(args, got, err)
		}
	}
	if len(seen) != 2 {
		t.Fatal(seen)
	}
	if seen[0].ID != remoteBoost || seen[0].ExpectedRevision != 3 || seen[0].Reason != "oops" || seen[0].IdempotencyKey != "" {
		t.Fatal(seen[0])
	}
	if seen[1].IdempotencyKey != "misfire-key" || seen[1].ID != "" || seen[1].ExpectedRevision != 0 {
		t.Fatal(seen[1])
	}
}

func TestWithdrawDraftRejectsAmbiguousAddressing(t *testing.T) {
	h := directBoostHandler(t)
	h.BoostWithdraw = func(context.Context, aiboost.Withdrawal) (aiboost.Draft, error) {
		t.Fatal("invalid input reached upstream")
		return aiboost.Draft{}, nil
	}
	for _, args := range []string{
		`{}`,
		`{"reason":"oops"}`,
		`{"id":"` + remoteBoost + `","expectedRevision":1,"idempotencyKey":"key"}`,
		`{"id":"` + remoteBoost + `"}`,
		`{"id":"` + remoteBoost + `","expectedRevision":0}`,
		`{"id":"` + remoteBoost + `","expectedRevision":-1}`,
		`{"id":"../withdraw","expectedRevision":1}`,
		`{"idempotencyKey":"key","expectedRevision":1}`,
		`{"idempotencyKey":"` + strings.Repeat("k", 129) + `"}`,
		`{"idempotencyKey":"bad\nkey"}`,
		`{"id":"` + remoteBoost + `","expectedRevision":1,"reason":"` + strings.Repeat("r", 10001) + `"}`,
		`{"id":"` + remoteBoost + `","expectedRevision":1,"owner":"spoof"}`,
		`{"id":"` + remoteBoost + `","expectedRevision":1,"status":"converted"}`,
		`[]`,
		`null`,
	} {
		_, err := h.tools["boost_withdraw_draft"].Call(boostCaller, json.RawMessage(args))
		if err == nil || err.Code != codeInvalidParams {
			t.Fatal(args, err)
		}
	}
}

func TestWithdrawDraftFailureMapping(t *testing.T) {
	h := directBoostHandler(t)
	args := json.RawMessage(`{"id":"` + remoteBoost + `","expectedRevision":1}`)
	if _, err := h.tools["boost_withdraw_draft"].Call(boostCaller, args); err == nil {
		t.Fatal("missing connection accepted")
	}
	cases := []struct {
		upstream error
		want     string
	}{
		{&aiboost.APIError{Status: 403}, "missing drafts:write"},
		{&aiboost.APIError{Status: 404}, "no draft for that id or submission key"},
		{&aiboost.APIError{Status: 409}, "do not retry blindly"},
		{&aiboost.APIError{Status: 429}, "rate limit"},
		{aiboost.ErrReconnect, "needs sign-in"},
		{errors.New("sensitive credential"), "not automatically retried"},
	}
	for _, tc := range cases {
		h.BoostWithdraw = func(context.Context, aiboost.Withdrawal) (aiboost.Draft, error) {
			return aiboost.Draft{}, tc.upstream
		}
		_, err := h.tools["boost_withdraw_draft"].Call(boostCaller, args)
		if err == nil || !strings.Contains(err.Message, tc.want) || strings.Contains(err.Message, "sensitive") {
			t.Fatal(tc.upstream, err)
		}
	}
}

func TestWithdrawDraftIsCredentialScanned(t *testing.T) {
	h := directBoostHandler(t)
	var err error
	h.Patterns, err = patterns.Load("")
	if err != nil {
		t.Fatal(err)
	}
	h.BoostWithdraw = func(context.Context, aiboost.Withdrawal) (aiboost.Draft, error) {
		t.Fatal("credential reached provider")
		return aiboost.Draft{}, nil
	}
	args := `{"id":"` + remoteBoost + `","expectedRevision":1,"reason":"sk-` + strings.Repeat("x", 30) + `"}`
	params, _ := json.Marshal(map[string]any{"name": "boost_withdraw_draft", "arguments": json.RawMessage(args)})
	raw, _ := json.Marshal(h.callTool(boostCaller, rpcRequest{Params: params}))
	if !strings.Contains(string(raw), "credential pattern blocked") || strings.Contains(string(raw), "sk-") {
		t.Fatal(string(raw))
	}
}

func TestListDraftsAcceptsWithdrawn(t *testing.T) {
	h := directBoostHandler(t)
	h.BoostListDrafts = func(_ context.Context, status, _ string) (aiboost.DraftPage, error) {
		if status != "withdrawn" {
			t.Fatal(status)
		}
		return aiboost.DraftPage{}, nil
	}
	if _, err := h.tools["boost_list_drafts"].Call(boostCaller, json.RawMessage(`{"status":"withdrawn"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.tools["boost_list_drafts"].Call(boostCaller, json.RawMessage(`{"status":"Withdrawn"}`)); err == nil || err.Code != codeInvalidParams {
		t.Fatal("status vocabulary must stay exact")
	}
}
