package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"piso/gateway/internal/aiboost"
	"piso/gateway/internal/store"
)

func (h *Handler) registerBoostDrafts() {
	str := func(description string, max int) map[string]any {
		return map[string]any{"type": "string", "description": description, "minLength": 1, "maxLength": max}
	}
	id := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description, "pattern": boostID.String()}
	}
	h.Register(Tool{
		Name:        "boost_list_drafts",
		Description: "List new-booster and new-version drafts in the gateway's connected AI Boost grant, not published library boosters. Optional status filter; up to 25 items with nextCursor/previousCursor. Pass the same status with a returned cursor. Returns type, current payload, revision, idempotencyKey, review, decision and withdrawal metadata.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": aiboost.ValidDraftStatuses, "description": "Omit to list all statuses; withdrawn is terminal."},
			"cursor": str("Opaque cursor returned by this tool, not a URL.", 4096),
		}}, Call: h.toolBoostListDrafts,
	})
	h.Register(Tool{
		Name:        "boost_get_draft",
		Description: "Get an AI Boost draft by draft UUID (not booster ID), including full Markdown, type, latest revision, version proposal fields and decision. Read before editing or after a conflict/ambiguous failure; never guess expectedRevision.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"id"}, "properties": map[string]any{
			"id": id("Draft UUID returned by list or submission."),
		}}, Call: h.toolBoostGetDraft,
	})
	h.Register(Tool{
		Name:        "boost_withdraw_draft",
		Description: "Retract a draft submitted in error through this gateway connection, before any human decision. Withdrawal is terminal and is not a review decision: it approves nothing, publishes nothing, and needs no reviewer. Pass id with expectedRevision to withdraw one exact revision, or idempotencyKey alone to retract the draft that key submitted when you no longer have its id (the key form takes no expectedRevision). Drafts are not isolated per worker: every worker on this gateway shares one library grant, so id and idempotencyKey can address any submitted draft here, not only one you created. Read with boost_get_draft first if you are not certain it is the draft you mean; submission keys are shared too, so a key you did not mint can resolve to another worker's draft. Only submitted drafts can be withdrawn; approved or rejected drafts conflict. Never retried automatically: after a conflict or ambiguous failure call boost_get_draft and re-read before acting. A withdrawn draft keeps its key forever, so a corrected resubmission needs a new one.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"id":               id("Draft UUID from submission or boost_list_drafts; use with expectedRevision."),
			"expectedRevision": map[string]any{"type": "integer", "minimum": 1, "description": "Last observed revision; required with id and never sent with idempotencyKey."},
			"idempotencyKey":   str("Submission key of the draft to retract; use instead of id when the id was lost. Keys are shared across this gateway's workers, so a key you did not mint may identify another worker's draft.", 128),
			"reason":           map[string]any{"type": "string", "maxLength": 10000, "description": "Optional short explanation recorded with the withdrawal."},
		}, "oneOf": []any{
			map[string]any{"required": []string{"id", "expectedRevision"}},
			map[string]any{"required": []string{"idempotencyKey"}},
		}}, Write: true, Call: h.toolBoostWithdrawDraft,
	})
	h.Register(Tool{
		Name:        "boost_draft_version",
		Description: "Propose a new version of an existing private, account-owned, free inline-Markdown booster in the connected library. Obtain baseVersionId from boost_get's versionId. AI Boost checks current base, increased SemVer and changed content. Does not approve or publish. Persist idempotencyKey and reuse it with the identical payload on retries; a changed proposal needs a new key.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false,
			"required": []string{"boosterId", "baseVersionId", "version", "content", "changelog", "rationale", "idempotencyKey"},
			"properties": map[string]any{
				"boosterId":     id("Existing AI Boost booster UUID."),
				"baseVersionId": id("Latest published version UUID from boost_get."),
				"version":       str("Proposed strict SemVer, greater than the base version.", 32),
				"content": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"format", "body"}, "properties": map[string]any{
					"format": map[string]any{"type": "string", "enum": []string{"markdown"}},
					"body":   str("Nonblank Markdown, at most 100000 UTF-8 bytes; whitespace preserved.", 100000),
				}},
				"changelog":      str("What changed.", 10000),
				"rationale":      str("Why this version is useful.", 10000),
				"idempotencyKey": str("Stable submission key, printable ASCII; reuse it with the same payload on retries and mint a new one for a changed proposal. Keys are shared by every worker on this gateway, so use a UUID rather than a short name.", 128),
			}}, Write: true, Call: h.toolBoostDraftVersion,
	})
}

func (h *Handler) toolBoostListDrafts(_ store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		Status string `json:"status"`
		Cursor string `json:"cursor"`
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	if len(in.Cursor) > 4096 || (in.Status != "" && !aiboost.ValidDraftStatus(in.Status)) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid draft status or cursor bounds"}
	}
	if h.BoostListDrafts == nil {
		return nil, boostUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	page, err := h.BoostListDrafts(ctx, in.Status, in.Cursor)
	if err != nil {
		return nil, boostError(err)
	}
	if page.Items == nil {
		page.Items = []aiboost.Draft{}
	}
	return page, nil
}

func (h *Handler) toolBoostGetDraft(_ store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		ID string `json:"id"`
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	if !boostID.MatchString(in.ID) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "AI Boost draft UUID required"}
	}
	if h.BoostGetDraft == nil {
		return nil, boostUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := h.BoostGetDraft(ctx, in.ID)
	if err != nil {
		return nil, boostError(err)
	}
	return out, nil
}

func (h *Handler) toolBoostWithdrawDraft(_ store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		ID               string `json:"id"`
		ExpectedRevision int    `json:"expectedRevision"`
		IdempotencyKey   string `json:"idempotencyKey"`
		Reason           string `json:"reason"`
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	// Fail closed on the shape before any network call: exactly one addressing
	// form, and a revision only alongside the id form. The client re-checks.
	w := aiboost.Withdrawal{
		ID: in.ID, IdempotencyKey: in.IdempotencyKey, ExpectedRevision: in.ExpectedRevision, Reason: in.Reason,
	}
	if err := w.Validate(); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if h.BoostWithdraw == nil {
		return nil, boostUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := h.BoostWithdraw(ctx, w)
	if err != nil {
		var upstream *aiboost.APIError
		if errors.As(err, &upstream) {
			switch upstream.Status {
			case 403:
				return nil, &rpcError{Code: codeServer, Message: "AI Boost denied the withdrawal: this connection's grant is missing drafts:write; reconnect AI Boost in the dashboard to consent to draft write access"}
			case 404:
				return nil, &rpcError{Code: codeServer, Message: "AI Boost has no draft for that id or submission key in the connected library"}
			case 409:
				return nil, &rpcError{Code: codeServer, Message: "AI Boost conflicts with this withdrawal: the draft was already decided or withdrawn, or the revision is stale; use boost_get_draft and do not retry blindly"}
			case 429:
				return nil, &rpcError{Code: codeServer, Message: "AI Boost rate limit reached; check the draft with boost_get_draft before retrying later"}
			}
		}
		e := boostError(err)
		e.Message += "; withdrawal was not automatically retried; check the draft before retrying"
		return nil, e
	}
	return out, nil
}

func (h *Handler) toolBoostDraftVersion(_ store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		aiboost.VersionDraft
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	if err := in.VersionDraft.Validate(in.IdempotencyKey); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if h.BoostDraftVersion == nil {
		return nil, boostUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := h.BoostDraftVersion(ctx, in.VersionDraft, in.IdempotencyKey)
	if err != nil {
		return nil, boostError(err)
	}
	return out, nil
}
