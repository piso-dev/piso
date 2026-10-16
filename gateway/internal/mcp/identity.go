package mcp

import (
	"encoding/json"

	"piso/gateway/internal/store"
)

func (h *Handler) registerIdentity() {
	h.Register(Tool{
		Name:        "capabilities",
		Description: "Whether egress is on, and the placeholder names this worker may see.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Call:        h.toolCapabilities,
	})
}

func (h *Handler) toolCapabilities(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	slug := caller.Slug
	scoped := []string{}
	if h.Store != nil {
		for _, sec := range h.Store.Secrets() {
			if store.SecretVisibleToWorker(sec, slug) {
				scoped = append(scoped, sec.Placeholder)
			}
		}
	}
	return map[string]any{
		"worker":             caller.Name,
		"slug":               slug,
		"internetEnabled":    !caller.InternetDisabled,
		"egressProxy":        "gateway:8080",
		"scopedPlaceholders": scoped,
	}, nil
}
