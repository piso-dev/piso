package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"piso/gateway/internal/store"
)

func (h *Handler) registerIngress() {
	h.Register(Tool{
		Name:        "ingress_request",
		Description: "Ask for the planning route. A human approves.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port": map[string]any{"type": "integer"},
				"note": map[string]any{"type": "string"},
			},
		},
		Write: true,
		Call:  h.toolIngressRequest,
	})
	h.Register(Tool{
		Name:        "ingress_status",
		Description: "Pending, approved, or none.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Call:        h.toolIngressStatus,
	})
	h.Register(Tool{
		Name:        "ingress_cancel",
		Description: "Drop the pending planning request.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Write:       true,
		Call:        h.toolIngressCancel,
	})
	h.Register(Tool{
		Name:        "routes",
		Description: "Names and ports already published for this worker.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Call:        h.toolRoutes,
	})
}

func planningURL(name string) string {
	port := 8082
	if v := strings.TrimSpace(os.Getenv("PISO_INGRESS_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 65535 {
			port = n
		}
	}
	q := url.QueryEscape(name)
	if port == 80 {
		return fmt.Sprintf("http://piso.local/?route=%s", q)
	}
	return fmt.Sprintf("http://piso.local:%d/?route=%s", port, q)
}

func ingressView(rec store.IngressRequestRec, created bool) map[string]any {
	out := map[string]any{
		"id":     rec.ID,
		"kind":   rec.Kind,
		"status": rec.Status,
		"port":   rec.Port,
		"name":   rec.Name,
		"url":    planningURL(rec.Name),
	}
	if rec.Note != "" {
		out["note"] = rec.Note
	}
	if created {
		out["created"] = true
	}
	return out
}

func (h *Handler) toolIngressRequest(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	if h.Store == nil {
		return nil, &rpcError{Code: codeInternal, Message: "store unavailable"}
	}
	var in struct {
		Port int    `json:"port"`
		Note string `json:"note"`
	}
	_ = json.Unmarshal(args, &in)
	if in.Port == 0 {
		in.Port = store.DefaultPlanningPort
	}
	if in.Port < 1 || in.Port > 65535 {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid port"}
	}
	slug := caller.Slug
	rec := store.IngressRequestRec{
		ID:     "ing_" + randHex(8),
		Kind:   store.IngressKindPlanning,
		Status: store.IngressStatusPending,
		Worker: caller.Name,
		Slug:   slug,
		Port:   in.Port,
		Name:   store.PlanningRouteName(slug),
		Note:   store.SanitizeLabel(in.Note, 200),
	}
	stored, created, err := h.Store.UpsertPendingIngress(rec)
	if err != nil {
		return nil, &rpcError{Code: codeServer, Message: err.Error()}
	}
	return ingressView(stored, created), nil
}

func (h *Handler) toolIngressStatus(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	if h.Store == nil {
		return map[string]any{"status": "none"}, nil
	}
	for _, rec := range h.Store.PendingIngressForWorker(caller.Name) {
		if rec.Kind == store.IngressKindPlanning {
			return ingressView(rec, false), nil
		}
	}
	name := store.PlanningRouteName(caller.Slug)
	if route, ok := h.Store.RouteByName(name); ok && route.Worker == caller.Name {
		return map[string]any{
			"kind":   store.IngressKindPlanning,
			"status": "approved",
			"port":   route.Port,
			"name":   name,
			"url":    planningURL(name),
		}, nil
	}
	return map[string]any{"status": "none"}, nil
}

func (h *Handler) toolIngressCancel(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	if h.Store == nil {
		return nil, &rpcError{Code: codeInternal, Message: "store unavailable"}
	}
	n := h.Store.CancelPendingIngress(caller.Name, store.IngressKindPlanning)
	return map[string]any{"ok": true, "cancelled": n}, nil
}

func (h *Handler) toolRoutes(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	if h.Store == nil {
		return map[string]any{"routes": out}, nil
	}
	for _, r := range h.Store.Routes() {
		if r.Worker != caller.Name {
			continue
		}
		row := map[string]any{
			"name":     r.Name,
			"port":     r.Port,
			"origin":   r.Origin,
			"disabled": r.Disabled,
			"url":      planningURL(r.Name),
		}
		if r.Note != "" {
			row["note"] = r.Note
		}
		out = append(out, row)
	}
	return map[string]any{"routes": out}, nil
}
