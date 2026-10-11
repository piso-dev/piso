package mcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"piso/gateway/internal/store"
	gtwc "piso/gateway/internal/technocore"
)

// CoordinatorDo signs a request to the coordinator. Tests inject a fake.
// A nil function, an empty caller PrincipalID, or ErrUnpaired is unpaired.
type CoordinatorDo func(method, path string, query url.Values, body []byte) (status int, payload []byte, err error)

func (h *Handler) registerInbox() {
	idSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string"},
		},
		"required": []string{"id"},
	}
	h.Register(Tool{
		Name:        "inbox_list",
		Description: "Runnable work this worker may claim or resume. Excludes waiting, terminal, and other workers’ leased tasks. Capped at 50 by default; pass limit (max 200).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{"type": "string"},
				"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
			},
		},
		Call: h.toolInboxList,
	})
	h.Register(Tool{
		Name:        "inbox_get",
		Description: "Read authorized task context in any state: directives, feedback, waits, checkpoint, artifacts, and execution history.",
		InputSchema: idSchema,
		Call:        h.toolInboxGet,
	})
	// All writes carry an idempotency key; attemptId fences every execution write.
	for _, spec := range []struct {
		name        string
		description string
		required    []string
	}{
		{"claim", "Claim runnable work atomically. Returns attemptId; a conflict is normal.", nil},
		{"renew", "Renew the current unexpired execution lease.", []string{"attemptId"}},
		{"update", "Persist findings, artifacts, and a handoff checkpoint.", []string{"attemptId", "summary"}},
		{"wait", "Atomically checkpoint, create a wait, and end execution. Exit after success.", []string{"attemptId", "summary", "checkpoint", "reason"}},
		{"release", "Checkpoint and give runnable work back without blocking it.", []string{"attemptId", "summary", "checkpoint"}},
		{"complete", "Finish work whose acceptance criteria are met; a PR normally waits for review instead.", []string{"attemptId", "summary"}},
		{"fail", "Terminal failure of the goal, supported by evidence. Use wait/stuck for uncertainty.", []string{"attemptId", "summary", "evidence"}},
	} {
		op := spec.name
		properties := map[string]any{}
		for _, key := range []string{"id", "requestId", "attemptId", "summary", "checkpoint", "question", "evidence"} {
			properties[key] = map[string]any{"type": "string"}
		}
		properties["reason"] = map[string]any{"type": "string", "enum": []string{"review", "needs_input", "external_dependency", "stuck"}}
		properties["ttl"] = map[string]any{"type": "integer", "minimum": 60, "maximum": 86400}
		properties["artifacts"] = map[string]any{"type": "array", "items": map[string]any{"type": "object"}}
		required := append([]string{"id", "requestId"}, spec.required...)
		h.Register(Tool{
			Name: "task_" + op, Description: spec.description,
			InputSchema: map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false},
			Write:       true,
			Call: func(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
				var body map[string]any
				if err := json.Unmarshal(args, &body); err != nil || body == nil {
					return nil, &rpcError{Code: codeInvalidParams, Message: "object required"}
				}
				return h.postTask(caller, args, "/"+op, body)
			},
		})
	}
	h.Register(Tool{
		Name:        "directive_propose",
		Description: "Suggest a goal. It is not active until a human approves.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":      map[string]any{"type": "string"},
				"body":       map[string]any{"type": "string"},
				"completion": map[string]any{"type": "string"},
			},
			"required": []string{"title", "body"},
		},
		Write: true,
		Call:  h.toolDirectivePropose,
	})
}

func rejectWorkerArg(args json.RawMessage) *rpcError {
	if len(args) == 0 || string(args) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil
	}
	if _, ok := m["worker"]; ok {
		return &rpcError{Code: codeInvalidParams, Message: "worker argument is rejected"}
	}
	if _, ok := m["workerId"]; ok {
		return &rpcError{Code: codeInvalidParams, Message: "worker argument is rejected"}
	}
	return nil
}

func safeID(id string) (string, *rpcError) {
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsAny(id, "/?#") {
		return "", &rpcError{Code: codeInvalidParams, Message: "id required"}
	}
	return id, nil
}

func unpairedDoc() map[string]any {
	return map[string]any{"status": "unpaired", "tasks": []any{}}
}

func (h *Handler) coord(caller store.WorkerRec, method, path string, extra url.Values, body []byte) (int, map[string]any, *rpcError) {
	if strings.TrimSpace(caller.PrincipalID) == "" {
		return 0, nil, &rpcError{Code: codeServer, Message: "worker not enrolled with coordinator; local enrollment is pending"}
	}
	if h.CoordinatorDo == nil {
		return 0, unpairedDoc(), nil
	}
	q := url.Values{}
	q.Set("worker", caller.PrincipalID)
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	status, payload, err := h.CoordinatorDo(method, path, q, body)
	if err != nil {
		if errors.Is(err, gtwc.ErrUnpaired) {
			return 0, unpairedDoc(), nil
		}
		return 0, nil, &rpcError{Code: codeServer, Message: "coordinator unavailable"}
	}
	var doc map[string]any
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &doc)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return status, doc, nil
}

func coordErr(status int, doc map[string]any) *rpcError {
	msg := "coordinator error"
	if err, ok := doc["error"].(string); ok && err != "" {
		msg = err
	}
	switch status {
	case http.StatusNotFound:
		msg = "not found"
	case http.StatusConflict:
		msg = "conflict"
	case http.StatusBadRequest:
		if msg == "coordinator error" {
			msg = "invalid request"
		}
	}
	return &rpcError{Code: codeServer, Message: msg}
}

func (h *Handler) toolInboxList(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	extra := url.Values{}
	var in struct {
		Status string `json:"status"`
		Limit  int    `json:"limit"`
	}
	_ = json.Unmarshal(args, &in)
	if strings.TrimSpace(in.Status) != "" {
		extra.Set("status", strings.TrimSpace(in.Status))
	}
	if in.Limit > 0 {
		extra.Set("limit", strconv.Itoa(in.Limit))
	}
	status, doc, rpcErr := h.coord(caller, http.MethodGet, "/api/v1/inbox", extra, nil)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if status == 0 {
		return doc, nil
	}
	if status == http.StatusNotFound {
		return map[string]any{"status": "ok", "tasks": []any{}, "directives": []any{}}, nil
	}
	if status >= 400 {
		return nil, coordErr(status, doc)
	}
	if _, ok := doc["status"]; !ok {
		doc["status"] = "ok"
	}
	return doc, nil
}

func (h *Handler) toolInboxGet(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "id required"}
	}
	id, err := safeID(in.ID)
	if err != nil {
		return nil, err
	}
	status, doc, rpcErr := h.coord(caller, http.MethodGet, "/api/v1/inbox/"+id, nil, nil)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if status == 0 {
		return doc, nil
	}
	if status >= 400 {
		return nil, coordErr(status, doc)
	}
	return doc, nil
}

func (h *Handler) postTask(caller store.WorkerRec, args json.RawMessage, suffix string, body map[string]any) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "id required"}
	}
	id, err := safeID(in.ID)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(body)
	status, doc, rpcErr := h.coord(caller, http.MethodPost, "/api/v1/tasks/"+id+suffix, nil, raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if status == 0 {
		return doc, nil
	}
	if status >= 400 {
		return nil, coordErr(status, doc)
	}
	return doc, nil
}

// Named entry point retained for callers that claim directly rather than via tools/call.
func (h *Handler) toolTaskClaim(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var body map[string]any
	if err := json.Unmarshal(args, &body); err != nil || body == nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "object required"}
	}
	return h.postTask(caller, args, "/claim", body)
}

func (h *Handler) toolDirectivePropose(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	var in struct {
		Title      string `json:"title"`
		Body       string `json:"body"`
		Completion string `json:"completion"`
	}
	if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Body) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "title and body required"}
	}
	body := map[string]any{"title": strings.TrimSpace(in.Title), "body": strings.TrimSpace(in.Body)}
	if strings.TrimSpace(in.Completion) != "" {
		body["completion"] = strings.TrimSpace(in.Completion)
	}
	raw, _ := json.Marshal(body)
	status, doc, rpcErr := h.coord(caller, http.MethodPost, "/api/v1/directives", nil, raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if status == 0 {
		return doc, nil
	}
	if status >= 400 {
		return nil, coordErr(status, doc)
	}
	return doc, nil
}
