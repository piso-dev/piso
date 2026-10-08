package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"piso/gateway/internal/aiboost"
	"piso/gateway/internal/store"
)

var boostID = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func (h *Handler) registerBoost() {
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	h.Register(Tool{Name: "boost_search", Description: "Search the coordinator catalog of boosters this worker may read. Returns local IDs for boost_get; does not search the account-wide AI Boost library.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"q": str("Optional title, slug or collection search (up to 500 characters)."), "offset": map[string]any{"type": "integer", "minimum": 0}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 25}}}, Call: h.toolBoostSearch})
	h.Register(Tool{Name: "boost_get", Description: "Read a permitted booster using its coordinator ID from boost_search. Fetches current AI Boost Markdown through the gateway; returns an excerpt capped at 8000 characters and the full content hash.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"id": str("Coordinator booster ID, not an AI Boost ID.")}, "required": []string{"id"}}, Call: h.toolBoostGet})
	h.Register(Tool{Name: "boost_suggest", Description: "Persist a booster proposal for human approval in the coordinator. Does not send to AI Boost, approve, or publish. Check the returned approvalId with approval_status instead of resubmitting.", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"booster": str("Optional coordinator booster ID to change."), "title": str("Proposal title (up to 300 characters)."), "body": str("Proposed Markdown or diff (up to 20000 characters)."), "why": str("Rationale (up to 500 characters)."), "collection": str("Optional collection label (not an authorization grant or publishing destination).")}, "required": []string{"title", "body"}}, Write: true, Call: h.toolBoostSuggest})
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

// Every operation uses the envelope principal through coord. Missing pairing,
// missing grants and coordinator failures never fall back to account-wide reads.
func (h *Handler) boostCoord(caller store.WorkerRec, method, path string, body []byte) (map[string]any, *rpcError) {
	status, doc, err := h.coord(caller, method, path, nil, body)
	if err != nil {
		return nil, err
	}
	if status == 0 {
		return nil, &rpcError{Code: codeServer, Message: "coordinator unpaired"}
	}
	if status < 200 || status >= 300 {
		return nil, coordErr(status, doc)
	}
	return doc, nil
}

// Whitelist catalog fields rather than relaying arbitrary coordinator metadata.
func boostMetadata(row map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"id", "slug", "title", "collection", "version", "contentHash"} {
		if v, ok := row[key].(string); ok {
			out[key] = v
		}
	}
	return out
}

func (h *Handler) toolBoostSearch(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		Q      string `json:"q"`
		Offset int    `json:"offset"`
		Limit  *int   `json:"limit"`
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	limit := 25
	if in.Limit != nil {
		limit = *in.Limit
	}
	if utf8.RuneCountInString(in.Q) > 500 || in.Offset < 0 || limit < 1 || limit > 25 {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid search bounds"}
	}
	doc, err := h.boostCoord(caller, http.MethodGet, "/api/v1/boosters", nil)
	if err != nil {
		return nil, err
	}
	rows, ok := doc["boosters"].([]any)
	if !ok {
		return nil, &rpcError{Code: codeServer, Message: "invalid coordinator catalog"}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, _ := rows[i].(map[string]any)
		b, _ := rows[j].(map[string]any)
		ai, _ := a["id"].(string)
		bi, _ := b["id"].(string)
		return ai < bi
	})
	query := strings.ToLower(strings.TrimSpace(in.Q))
	matches := 0
	items := []map[string]any{}
	for _, v := range rows {
		row, ok := v.(map[string]any)
		if !ok {
			continue
		}
		metadata := boostMetadata(row)
		haystack := ""
		for _, key := range []string{"title", "slug", "collection"} {
			if s, ok := metadata[key].(string); ok {
				haystack += " " + s
			}
		}
		if !strings.Contains(strings.ToLower(haystack), query) {
			continue
		}
		if matches >= in.Offset && len(items) < limit {
			items = append(items, metadata)
		}
		matches++
	}
	var next any
	if matches > in.Offset && matches-in.Offset > len(items) {
		next = in.Offset + len(items)
	}
	return map[string]any{"status": "ok", "source": "coordinator", "boosters": items, "nextOffset": next}, nil
}

func (h *Handler) toolBoostGet(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		ID string `json:"id"`
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	id := in.ID
	if !boostID.MatchString(id) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "coordinator booster UUID required"}
	}
	doc, err := h.boostCoord(caller, http.MethodGet, "/api/v1/boosters/"+id, nil)
	if err != nil {
		return nil, err
	}
	if doc["id"] != id {
		return nil, &rpcError{Code: codeServer, Message: "invalid coordinator booster"}
	}
	upstream, ok := doc["aiBoostId"].(string)
	if !ok || upstream == "" {
		return nil, &rpcError{Code: codeServer, Message: "booster has no AI Boost catalog mapping"}
	}
	if h.BoostGet == nil {
		return nil, &rpcError{Code: codeServer, Message: "AI Boost unavailable; connect in the dashboard"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	booster, fetchErr := h.BoostGet(ctx, upstream)
	if fetchErr != nil {
		msg := "AI Boost fetch failed"
		if errors.Is(fetchErr, aiboost.ErrReconnect) {
			msg = "AI Boost needs sign-in in the dashboard"
		}
		return nil, &rpcError{Code: codeServer, Message: msg}
	}
	if booster.ID != upstream || booster.Content.Format != "markdown" {
		return nil, &rpcError{Code: codeServer, Message: "invalid AI Boost booster"}
	}
	excerpt := booster.Excerpt()
	out := boostMetadata(doc)
	out["title"] = excerpt.Name
	out["version"] = excerpt.Version
	out["contentHash"] = excerpt.SHA256
	out["excerpt"] = excerpt.Text
	out["truncated"] = excerpt.Truncated
	out["status"] = "ok"
	return out, nil
}

func (h *Handler) toolBoostSuggest(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		Booster    string `json:"booster,omitempty"`
		Title      string `json:"title"`
		Body       string `json:"body"`
		Why        string `json:"why,omitempty"`
		Collection string `json:"collection,omitempty"`
	}
	if err := boostArgs(args, &in); err != nil {
		return nil, err
	}
	in.Title = strings.TrimSpace(in.Title)
	in.Body = strings.TrimSpace(in.Body)
	if in.Title == "" || in.Body == "" || utf8.RuneCountInString(in.Title) > 300 || utf8.RuneCountInString(in.Body) > 20000 || utf8.RuneCountInString(in.Why) > 500 || utf8.RuneCountInString(in.Collection) > 120 {
		return nil, &rpcError{Code: codeInvalidParams, Message: "title and body required within proposal size limits"}
	}
	if in.Booster != "" && !boostID.MatchString(in.Booster) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "coordinator booster UUID required"}
	}
	raw, _ := json.Marshal(in)
	return h.boostCoord(caller, http.MethodPost, "/api/v1/boosters/proposals", raw)
}
