package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"time"
	"unicode/utf8"

	"piso/gateway/internal/aiboost"
	"piso/gateway/internal/store"
)

var boostID = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func (h *Handler) registerBoost() {
	h.registerBoostEditDraft()
	h.registerBoostDrafts()
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	h.Register(Tool{Name: "boost_search", Description: "Search AI Boost's shared library using gateway credentials. Returns AI Boost IDs and cursor pagination (up to 25 items). No coordinator catalog or grants required.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"q": str("Optional name/description search, up to 500 characters."), "cursor": str("Cursor from nextCursor or previousCursor, not a URL.")}}, Call: h.toolBoostSearch})
	h.Register(Tool{Name: "boost_get", Description: "Get current metadata and full Markdown directly from AI Boost using an ID returned by boost_search.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"id": str("AI Boost booster UUID.")}, "required": []string{"id"}}, Call: h.toolBoostGet})
	h.Register(Tool{Name: "boost_draft", Description: "Submit a new booster draft to AI Boost for review there; does not approve or publish. Reuse the same idempotencyKey and payload on retries; use a new key for a new submission. Returns AI Boost's draft ID, status and revision.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"name": str("Booster name, 1–255 characters."), "description": str("Description, 1–10000 characters."),
		"content":       map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"format": map[string]any{"type": "string", "enum": []string{"markdown"}}, "body": str("Nonblank Markdown, at most 100000 UTF-8 bytes. Whitespace is preserved.")}, "required": []string{"format", "body"}},
		"suggestedTags": map[string]any{"type": "array", "maxItems": 30, "items": map[string]any{"type": "string", "maxLength": 100}},
		"rationale":     str("Why this booster is useful, 1–10000 characters."), "idempotencyKey": str("Stable unique submission key, 1–128 characters (for example a UUID). Persist and reuse on retries.")}, "required": []string{"name", "description", "content", "rationale", "idempotencyKey"}}, Write: true, Call: h.toolBoostDraft})
}

func boostArgs(raw json.RawMessage, out any) *rpcError {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return &rpcError{Code: codeInvalidParams, Message: "object arguments required"}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return &rpcError{Code: codeInvalidParams, Message: "invalid or unknown boost arguments"}
	}
	if dec.Decode(new(any)) != io.EOF {
		return &rpcError{Code: codeInvalidParams, Message: "invalid boost arguments"}
	}
	return nil
}

func boostError(err error) *rpcError {
	message := "AI Boost request failed"
	if errors.Is(err, aiboost.ErrReconnect) {
		message = "AI Boost needs sign-in in the dashboard"
	}
	var apiErr *aiboost.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case 400:
			message = "AI Boost rejected the request payload"
		case 403:
			message = "AI Boost credential does not permit this operation"
		case 404:
			message = "AI Boost item not found in the connected library"
		case 409:
			message = "AI Boost conflict: submission key, base version or proposed version conflicts with current state; inspect the draft and booster before retrying"
		case 429:
			message = "AI Boost rate limit reached; retry later with the same submission key and payload"
		}
	}
	// Never relay upstream response bodies or transport errors containing credentials.
	return &rpcError{Code: codeServer, Message: message}
}

func boostUnavailable() *rpcError {
	return &rpcError{Code: codeServer, Message: "AI Boost unavailable; connect in the dashboard"}
}

// Extract only an opaque cursor. Never follow provider-supplied pagination URLs
// or send gateway credentials to their host.
func boostCursor(link *string) (any, error) {
	if link == nil {
		return nil, nil
	}
	u, err := url.Parse(*link)
	if err != nil {
		return nil, err
	}
	cursor := u.Query().Get("cursor")
	if cursor == "" || len(cursor) > 4096 {
		return nil, errors.New("invalid cursor")
	}
	return cursor, nil
}

func (h *Handler) toolBoostSearch(_ store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		Q      string `json:"q"`
		Cursor string `json:"cursor"`
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(in.Q) > 500 || len(in.Cursor) > 4096 {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid search bounds"}
	}
	if h.BoostList == nil {
		return nil, boostUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	page, err := h.BoostList(ctx, in.Q, in.Cursor)
	if err != nil {
		return nil, boostError(err)
	}
	next, err := boostCursor(page.Next)
	if err != nil {
		return nil, boostError(err)
	}
	previous, err := boostCursor(page.Previous)
	if err != nil {
		return nil, boostError(err)
	}
	if page.Items == nil {
		page.Items = []aiboost.Booster{}
	}
	return map[string]any{"items": page.Items, "nextCursor": next, "previousCursor": previous}, nil
}

func (h *Handler) toolBoostGet(_ store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		ID string `json:"id"`
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	if !boostID.MatchString(in.ID) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "AI Boost booster UUID required"}
	}
	if h.BoostGet == nil {
		return nil, boostUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	booster, err := h.BoostGet(ctx, in.ID)
	if err != nil {
		return nil, boostError(err)
	}
	if booster.ID != in.ID || booster.Content.Format != "markdown" {
		return nil, boostError(errors.New("invalid booster"))
	}
	return booster, nil
}

func (h *Handler) toolBoostDraft(_ store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		aiboost.DraftPayload
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	if err := in.DraftPayload.Validate(in.IdempotencyKey); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if in.SuggestedTags == nil {
		in.SuggestedTags = []string{}
	}
	if h.BoostDraft == nil {
		return nil, boostUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	draft, err := h.BoostDraft(ctx, in.DraftPayload, in.IdempotencyKey)
	if err != nil {
		return nil, boostError(err)
	}
	return draft, nil
}
