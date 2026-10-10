package mcp

import (
	"context"
	"encoding/json"
	"time"

	"piso/gateway/internal/aiboost"
	"piso/gateway/internal/store"
)

func (h *Handler) registerBoostSuggestions() {
	str := func(description string, max int) map[string]any {
		return map[string]any{"type": "string", "description": description, "minLength": 1, "maxLength": max}
	}
	id := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description, "pattern": boostID.String()}
	}
	h.Register(Tool{
		Name:        "boost_list_suggestions",
		Description: "List new-booster and new-version suggestions in the gateway's connected AI Boost grant, not published library boosters. Optional status filter; up to 25 items with nextCursor/previousCursor. Pass the same status with a returned cursor. Returns type, current payload, revision, review and decision metadata.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"submitted", "converted", "rejected"}, "description": "Omit to list all statuses."},
			"cursor": str("Opaque cursor returned by this tool, not a URL.", 4096),
		}}, Call: h.toolBoostListSuggestions,
	})
	h.Register(Tool{
		Name:        "boost_get_suggestion",
		Description: "Get an AI Boost suggestion by suggestion UUID (not booster ID), including full Markdown, type, latest revision, version proposal fields and decision. Read before editing or after a conflict/ambiguous failure; never guess expectedRevision.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"id"}, "properties": map[string]any{
			"id": id("Suggestion UUID returned by list or submission."),
		}}, Call: h.toolBoostGetSuggestion,
	})
	h.Register(Tool{
		Name:        "boost_suggest_version",
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
				"idempotencyKey": str("Stable unique submission key; printable ASCII without spaces.", 128),
			}}, Write: true, Call: h.toolBoostSuggestVersion,
	})
}

func (h *Handler) toolBoostListSuggestions(_ store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		Status string `json:"status"`
		Cursor string `json:"cursor"`
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	if len(in.Cursor) > 4096 || (in.Status != "" && in.Status != "submitted" && in.Status != "converted" && in.Status != "rejected") {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid suggestion status or cursor bounds"}
	}
	if h.BoostListSuggestions == nil {
		return nil, boostUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	page, err := h.BoostListSuggestions(ctx, in.Status, in.Cursor)
	if err != nil {
		return nil, boostError(err)
	}
	if page.Items == nil {
		page.Items = []aiboost.Suggestion{}
	}
	return page, nil
}

func (h *Handler) toolBoostGetSuggestion(_ store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		ID string `json:"id"`
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	if !boostID.MatchString(in.ID) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "AI Boost suggestion UUID required"}
	}
	if h.BoostGetSuggestion == nil {
		return nil, boostUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := h.BoostGetSuggestion(ctx, in.ID)
	if err != nil {
		return nil, boostError(err)
	}
	return out, nil
}

func (h *Handler) toolBoostSuggestVersion(_ store.WorkerRec, args json.RawMessage) (any, *rpcError) {
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
	if h.BoostSuggestVersion == nil {
		return nil, boostUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := h.BoostSuggestVersion(ctx, in.VersionDraft, in.IdempotencyKey)
	if err != nil {
		return nil, boostError(err)
	}
	return out, nil
}
