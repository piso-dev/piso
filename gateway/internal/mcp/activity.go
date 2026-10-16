package mcp

import (
	"encoding/json"
	"strings"

	"piso/gateway/internal/store"
)

func (h *Handler) registerBoard() {
	h.Register(Tool{
		Name:        "activity_post",
		Description: "Post a board event: progress, milestone, reminder, poke, active, waiting, host, or note.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind":       map[string]any{"type": "string"},
				"text":       map[string]any{"type": "string"},
				"targetSlug": map[string]any{"type": "string"},
			},
			"required": []string{"kind", "text"},
		},
		Write: true,
		Call:  h.toolActivityPost,
	})
	h.Register(Tool{
		Name:        "activity_feed",
		Description: "This worker's own board rows.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind":  map[string]any{"type": "string"},
				"since": map[string]any{"type": "integer"},
			},
		},
		Call: h.toolActivityFeed,
	})
	h.Register(Tool{
		Name:        "host_request",
		Description: "Ask the host to run a command. The result comes back at a rendezvous path under /workspace.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":    map[string]any{"type": "string"},
				"resultPath": map[string]any{"type": "string"},
			},
			"required": []string{"command", "resultPath"},
		},
		Write: true,
		Call:  h.toolHostRequest,
	})
}

func agentActivityKind(k string) bool {
	switch k {
	case store.ActivityKindProgress, store.ActivityKindMilestone, store.ActivityKindReminder,
		store.ActivityKindPoke, store.ActivityKindActive, store.ActivityKindWaiting,
		store.ActivityKindHost, store.ActivityKindNote:
		return true
	}
	return false
}

func (h *Handler) insertBoard(caller store.WorkerRec, kind, text, target string) (any, *rpcError) {
	if h.Store == nil {
		return nil, &rpcError{Code: codeInternal, Message: "activity store unavailable"}
	}
	kind = strings.TrimSpace(kind)
	text = store.SanitizeLabel(text, 500)
	target = store.SanitizeLabel(target, 63)
	if !agentActivityKind(kind) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown kind"}
	}
	if text == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "text required"}
	}
	stored, err := h.Store.InsertActivity(store.Activity{
		Worker:     caller.Name,
		Slug:       caller.Slug,
		Kind:       kind,
		TargetSlug: target,
		Text:       text,
	})
	if err != nil {
		return nil, &rpcError{Code: codeServer, Message: err.Error()}
	}
	return map[string]any{"id": stored.ID, "ts": stored.Ts, "kind": stored.Kind}, nil
}

func (h *Handler) toolActivityPost(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	var in struct {
		Kind       string `json:"kind"`
		Text       string `json:"text"`
		TargetSlug string `json:"targetSlug"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "kind and text required"}
	}
	return h.insertBoard(caller, in.Kind, in.Text, in.TargetSlug)
}

func (h *Handler) toolHostRequest(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	var in struct {
		Command    string `json:"command"`
		ResultPath string `json:"resultPath"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "command and resultPath required"}
	}
	cmd := store.SanitizeLabel(in.Command, 400)
	path := store.SanitizeLabel(in.ResultPath, 200)
	if cmd == "" || path == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "command and resultPath required"}
	}
	text := cmd + " · result → " + path
	return h.insertBoard(caller, store.ActivityKindHost, text, "")
}

func (h *Handler) toolActivityFeed(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	if h.Store == nil {
		return nil, &rpcError{Code: codeInternal, Message: "activity store unavailable"}
	}
	var in struct {
		Kind  string `json:"kind"`
		Since int64  `json:"since"`
	}
	_ = json.Unmarshal(args, &in)
	f := store.ActivityFilter{
		Slug:    caller.Slug,
		Kind:    strings.TrimSpace(in.Kind),
		SinceMs: in.Since,
		Limit:   50,
	}
	if f.Kind != "" && !store.ValidActivityKind(f.Kind) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown kind"}
	}
	rows, err := h.Store.QueryActivities(f)
	if err != nil {
		return nil, &rpcError{Code: codeServer, Message: err.Error()}
	}
	now := h.Now().UnixNano() / 1000000
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		age := int64(0)
		if now > a.Ts {
			age = (now - a.Ts) / 60000
		}
		row := map[string]any{
			"id":     a.ID,
			"kind":   a.Kind,
			"text":   a.Text,
			"ts":     a.Ts,
			"ageMin": age,
		}
		if a.TargetSlug != "" {
			row["targetSlug"] = a.TargetSlug
		}
		out = append(out, row)
	}
	return map[string]any{"status": "ok", "rows": out}, nil
}
