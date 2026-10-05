package mcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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
		Description: "Tasks this worker holds or may claim, plus visible directives. Includes peek flags for the unattended watcher.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{"type": "string"},
			},
		},
		Call: h.toolInboxList,
	})
	h.Register(Tool{
		Name:        "inbox_get",
		Description: "One directive or task: body, plan, dependencies, budget, lease.",
		InputSchema: idSchema,
		Call:        h.toolInboxGet,
	})
	h.Register(Tool{
		Name:        "task_claim",
		Description: "Take a lease on a task. Conflict if someone else holds it.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":  map[string]any{"type": "string"},
				"ttl": map[string]any{"type": "integer"},
			},
			"required": []string{"id"},
		},
		Write: true,
		Call:  h.toolTaskClaim,
	})
	h.Register(Tool{
		Name:        "task_renew",
		Description: "Extend the lease on a task this worker holds.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":  map[string]any{"type": "string"},
				"ttl": map[string]any{"type": "integer"},
			},
			"required": []string{"id"},
		},
		Write: true,
		Call:  h.toolTaskRenew,
	})
	h.Register(Tool{
		Name:        "task_release",
		Description: "Give a claimed task back.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":     map[string]any{"type": "string"},
				"reason": map[string]any{"type": "string"},
			},
			"required": []string{"id"},
		},
		Write: true,
		Call:  h.toolTaskRelease,
	})
	h.Register(Tool{
		Name:        "task_complete",
		Description: "Finish a claimed task with a summary.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":      map[string]any{"type": "string"},
				"summary": map[string]any{"type": "string"},
			},
			"required": []string{"id", "summary"},
		},
		Write: true,
		Call:  h.toolTaskComplete,
	})
	h.Register(Tool{
		Name:        "task_fail",
		Description: "Stop a claimed task with a reason.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":     map[string]any{"type": "string"},
				"reason": map[string]any{"type": "string"},
			},
			"required": []string{"id", "reason"},
		},
		Write: true,
		Call:  h.toolTaskFail,
	})
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
	if strings.TrimSpace(caller.PrincipalID) == "" || h.CoordinatorDo == nil {
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
	}
	_ = json.Unmarshal(args, &in)
	if strings.TrimSpace(in.Status) != "" {
		extra.Set("status", strings.TrimSpace(in.Status))
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

func (h *Handler) toolTaskClaim(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		TTL *int `json:"ttl"`
	}
	_ = json.Unmarshal(args, &in)
	body := map[string]any{}
	if in.TTL != nil {
		body["ttl"] = *in.TTL
	}
	return h.postTask(caller, args, "/claim", body)
}

func (h *Handler) toolTaskRenew(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		TTL *int `json:"ttl"`
	}
	_ = json.Unmarshal(args, &in)
	body := map[string]any{}
	if in.TTL != nil {
		body["ttl"] = *in.TTL
	}
	return h.postTask(caller, args, "/renew", body)
}

func (h *Handler) toolTaskRelease(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(args, &in)
	body := map[string]any{}
	if strings.TrimSpace(in.Reason) != "" {
		body["reason"] = strings.TrimSpace(in.Reason)
	}
	return h.postTask(caller, args, "/release", body)
}

func (h *Handler) toolTaskComplete(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Summary) == "" {
		if err := rejectWorkerArg(args); err != nil {
			return nil, err
		}
		return nil, &rpcError{Code: codeInvalidParams, Message: "summary required"}
	}
	return h.postTask(caller, args, "/complete", map[string]any{"summary": strings.TrimSpace(in.Summary)})
}

func (h *Handler) toolTaskFail(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	var in struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Reason) == "" {
		if err := rejectWorkerArg(args); err != nil {
			return nil, err
		}
		return nil, &rpcError{Code: codeInvalidParams, Message: "reason required"}
	}
	return h.postTask(caller, args, "/fail", map[string]any{"reason": strings.TrimSpace(in.Reason)})
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
