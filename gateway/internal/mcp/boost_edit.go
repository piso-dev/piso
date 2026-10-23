package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"piso/gateway/internal/aiboost"
	"piso/gateway/internal/store"
)

func (h *Handler) registerBoostEditDraft() {
	str := func(description string, max int) map[string]any {
		return map[string]any{"type": "string", "description": description, "minLength": 1, "maxLength": max}
	}
	h.Register(Tool{
		Name:        "boost_edit_draft",
		Description: "Edit a submitted new-booster or new-version AI Boost draft using its ID and last observed revision. Version proposals accept baseVersionId, version, content, changelog, rationale; new boosters accept name, description, content, suggestedTags, rationale. Do not mix type-specific fields. Omitted fields stay unchanged; content replaces the full Markdown object; [] clears tags. Does not approve or publish. After a conflict or ambiguous failure, use boost_get_draft first; never guess a newer revision.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"id", "expectedRevision"}, "properties": map[string]any{
			"id":               map[string]any{"type": "string", "description": "Draft UUID returned by submission or boost_list_drafts (not a booster ID).", "pattern": boostID.String()},
			"expectedRevision": map[string]any{"type": "integer", "minimum": 1, "description": "Last observed draft revision; must still be submitted at this revision."},
			"changeSummary":    map[string]any{"type": "string", "maxLength": 2000, "description": "Optional revision note; not a payload change on its own."},
			"name":             str("Replacement name.", 255),
			"description":      str("Replacement description.", 10000),
			"rationale":        str("Replacement rationale.", 10000),
			"baseVersionId":    map[string]any{"type": "string", "pattern": boostID.String(), "description": "Rebase a version proposal onto this published version UUID."},
			"version":          str("Replacement strict SemVer for a version proposal.", 32),
			"changelog":        str("Replacement changelog for a version proposal.", 10000),
			"content": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"format", "body"}, "properties": map[string]any{
				"format": map[string]any{"type": "string", "enum": []string{"markdown"}},
				"body":   str("Nonblank Markdown, at most 100000 UTF-8 bytes; whitespace preserved.", 100000),
			}},
			"suggestedTags": map[string]any{"type": "array", "maxItems": 30, "items": str("Tag.", 100)},
		}, "anyOf": []any{
			map[string]any{"required": []string{"name"}}, map[string]any{"required": []string{"description"}},
			map[string]any{"required": []string{"content"}}, map[string]any{"required": []string{"suggestedTags"}},
			map[string]any{"required": []string{"rationale"}},
			map[string]any{"required": []string{"baseVersionId"}},
			map[string]any{"required": []string{"version"}},
			map[string]any{"required": []string{"changelog"}},
		}}, Write: true, Call: h.toolBoostEditDraft,
	})
}

func (h *Handler) toolBoostEditDraft(_ store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		ID string `json:"id"`
		aiboost.DraftEdit
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	// Go pointers otherwise conflate explicit null with omission. The upstream
	// PATCH contract rejects nulls; do not silently discard an intended change.
	var fields map[string]json.RawMessage
	if json.Unmarshal(args, &fields) == nil {
		for _, raw := range fields {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return nil, &rpcError{Code: codeInvalidParams, Message: "omit unchanged fields; null values are not allowed"}
			}
		}
	}
	if err := in.DraftEdit.Validate(in.ID); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if h.BoostEditDraft == nil {
		return nil, boostUnavailable()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := h.BoostEditDraft(ctx, in.ID, in.DraftEdit)
	if err != nil {
		var upstream *aiboost.APIError
		if errors.As(err, &upstream) {
			switch upstream.Status {
			case 401:
				return nil, &rpcError{Code: codeServer, Message: "AI Boost rejected edit authentication; check the connection and current draft before retrying"}
			case 409:
				return nil, &rpcError{Code: codeServer, Message: "AI Boost draft changed, was withdrawn, or was already decided; use boost_get_draft before editing again; do not guess a newer revision"}
			case 429:
				return nil, &rpcError{Code: codeServer, Message: "AI Boost rate limit reached; check the current draft before retrying later"}
			}
		}
		e := boostError(err)
		e.Message += "; edit was not automatically retried; check the current draft before retrying"
		return nil, e
	}
	return out, nil
}
