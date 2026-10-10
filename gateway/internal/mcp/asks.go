package mcp

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"piso/gateway/internal/store"
)

func (h *Handler) registerAsks() {
	idSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string"},
		},
		"required": []string{"id"},
	}
	h.Register(Tool{
		Name:        "approval_request",
		Description: "Ask a human to allow an action. With taskId, requires attemptId, requestId, summary, checkpoint; atomically waits and ends execution. Exit after success.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":  map[string]any{"type": "string"},
				"title":   map[string]any{"type": "string"},
				"payload": map[string]any{"type": "object"},
				"taskId": map[string]any{"type": "string"},
				"attemptId": map[string]any{"type": "string"},
				"requestId": map[string]any{"type": "string"},
				"summary": map[string]any{"type": "string"},
				"checkpoint": map[string]any{"type": "string"},
			},
			"required": []string{"action", "title"},
		},
		Write: true,
		Call:  h.toolApprovalRequest,
	})
	h.Register(Tool{
		Name:        "approval_list",
		Description: "Approvals this worker submitted, with status.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{"type": "string"},
			},
		},
		Call: h.toolApprovalList,
	})
	h.Register(Tool{
		Name:        "approval_status",
		Description: "Pending, approved, rejected, or expired.",
		InputSchema: idSchema,
		Call:        h.toolApprovalStatus,
	})
	h.Register(Tool{
		Name:        "human_action_request",
		Description: "Ask for an external action. With taskId, requires attemptId, requestId, summary, checkpoint; atomically waits and ends execution. Exit after success.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"category":     map[string]any{"type": "string"},
				"title":        map[string]any{"type": "string"},
				"instructions": map[string]any{"type": "string"},
				"verification": map[string]any{"type": "string"},
				"taskId":       map[string]any{"type": "string"},
				"attemptId": map[string]any{"type": "string"},
				"requestId": map[string]any{"type": "string"},
				"summary": map[string]any{"type": "string"},
				"checkpoint": map[string]any{"type": "string"},
			},
			"required": []string{"category", "title", "instructions"},
		},
		Write: true,
		Call:  h.toolHumanActionRequest,
	})
	h.Register(Tool{
		Name:        "human_action_list",
		Description: "Human actions this worker submitted, with status.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{"type": "string"},
			},
		},
		Call: h.toolHumanActionList,
	})
	h.Register(Tool{
		Name:        "human_action_status",
		Description: "Where that ask stands. Evidence, never host secrets.",
		InputSchema: idSchema,
		Call:        h.toolHumanActionStatus,
	})
}

func (h *Handler) listCoord(caller store.WorkerRec, args json.RawMessage, path, key string) (any, *rpcError) {
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
	status, doc, rpcErr := h.coord(caller, http.MethodGet, path, extra, nil)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if status == 0 {
		return doc, nil
	}
	if status == http.StatusNotFound {
		return map[string]any{"status": "ok", key: []any{}}, nil
	}
	if status >= 400 {
		return nil, coordErr(status, doc)
	}
	if _, ok := doc["status"]; !ok {
		doc["status"] = "ok"
	}
	return doc, nil
}

func (h *Handler) toolApprovalList(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	return h.listCoord(caller, args, "/api/v1/approvals", "approvals")
}

func (h *Handler) toolHumanActionList(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	return h.listCoord(caller, args, "/api/v1/human-actions", "humanActions")
}

func (h *Handler) postAsk(caller store.WorkerRec, args json.RawMessage, path string) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil { return nil, err }
	var body map[string]any
	if err := json.Unmarshal(args, &body); err != nil || body == nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "object required"}
	}
	if id, _ := body["taskId"].(string); id != "" {
		for _, key := range []string{"attemptId", "requestId", "summary", "checkpoint"} {
			value, _ := body[key].(string)
			if strings.TrimSpace(value) == "" {
				return nil, &rpcError{Code: codeInvalidParams, Message: key + " required for task-linked ask"}
			}
		}
	}
	status, doc, rpcErr := h.coord(caller, http.MethodPost, path, nil, args)
	if rpcErr != nil { return nil, rpcErr }
	if status >= 400 { return nil, coordErr(status, doc) }
	return doc, nil
}

func (h *Handler) toolApprovalRequest(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	return h.postAsk(caller, args, "/api/v1/approvals")
}

func (h *Handler) toolApprovalStatus(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
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
	status, doc, rpcErr := h.coord(caller, http.MethodGet, "/api/v1/approvals/"+id, nil, nil)
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

func (h *Handler) toolHumanActionRequest(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	return h.postAsk(caller, args, "/api/v1/human-actions")
}

func (h *Handler) toolHumanActionStatus(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
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
	status, doc, rpcErr := h.coord(caller, http.MethodGet, "/api/v1/human-actions/"+id, nil, nil)
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
