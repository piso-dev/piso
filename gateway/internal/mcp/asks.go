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
		Description: "Ask a human to allow an action.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":  map[string]any{"type": "string"},
				"title":   map[string]any{"type": "string"},
				"payload": map[string]any{"type": "object"},
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
		Description: "Ask a human to do something the system cannot do.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"category":     map[string]any{"type": "string"},
				"title":        map[string]any{"type": "string"},
				"instructions": map[string]any{"type": "string"},
				"verification": map[string]any{"type": "string"},
				"taskId":       map[string]any{"type": "string"},
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

func (h *Handler) toolApprovalRequest(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	var in struct {
		Action  string         `json:"action"`
		Title   string         `json:"title"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Action) == "" || strings.TrimSpace(in.Title) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "action and title required"}
	}
	body := map[string]any{"action": strings.TrimSpace(in.Action), "title": strings.TrimSpace(in.Title)}
	if in.Payload != nil {
		body["payload"] = in.Payload
	}
	raw, _ := json.Marshal(body)
	status, doc, rpcErr := h.coord(caller, http.MethodPost, "/api/v1/approvals", nil, raw)
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
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	var in struct {
		Category     string `json:"category"`
		Title        string `json:"title"`
		Instructions string `json:"instructions"`
		Verification string `json:"verification"`
		TaskID       string `json:"taskId"`
	}
	if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Category) == "" || strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Instructions) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "category, title, and instructions required"}
	}
	body := map[string]any{
		"category":     strings.TrimSpace(in.Category),
		"title":        strings.TrimSpace(in.Title),
		"instructions": strings.TrimSpace(in.Instructions),
	}
	if strings.TrimSpace(in.Verification) != "" {
		body["verification"] = strings.TrimSpace(in.Verification)
	}
	if strings.TrimSpace(in.TaskID) != "" {
		body["taskId"] = strings.TrimSpace(in.TaskID)
	}
	raw, _ := json.Marshal(body)
	status, doc, rpcErr := h.coord(caller, http.MethodPost, "/api/v1/human-actions", nil, raw)
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
