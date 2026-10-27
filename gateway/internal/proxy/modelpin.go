package proxy

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// modelPin adds a restriction for configured generation endpoints; it never
// overrides the scanner or domain policy. Catalog and unrelated traffic pass.
func (h *Handler) modelPin(req *http.Request, body []byte) (string, string) {
	if req.Method != http.MethodPost {
		return "", ""
	}
	isGeneration := false
	for _, provider := range h.Store.PiProviderStack() {
		base, err := url.Parse(provider.BaseURL)
		if err != nil || !strings.EqualFold(base.Host, req.URL.Host) || base.Scheme != req.URL.Scheme {
			continue
		}
		prefix := strings.TrimSuffix(base.Path, "/")
		path := strings.TrimSuffix(req.URL.Path, "/")
		for _, suffix := range []string{"/chat/completions", "/responses", "/messages", "/completions"} {
			if path == prefix+suffix {
				isGeneration = true
			}
		}
	}
	if !isGeneration {
		return "", ""
	}
	selected := h.Store.PiProvider()
	if id := h.Store.ModelOverrideProviderID(h.workerID(req)); id != "" {
		selected = h.Store.PiProviderByID(id)
	}
	if selected == nil {
		return "", "model-pin-unresolved"
	}
	base, err := url.Parse(selected.BaseURL)
	if err != nil || !strings.EqualFold(base.Host, req.URL.Host) || base.Scheme != req.URL.Scheme || !strings.HasPrefix(req.URL.Path, strings.TrimSuffix(base.Path, "/")+"/") {
		return "", "model-provider-mismatch"
	}
	expected := strings.TrimSpace(selected.DefaultModel)
	if expected == "" {
		for _, id := range selected.Models {
			if expected = strings.TrimSpace(id); expected != "" {
				break
			}
		}
	}
	var payload struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &payload) != nil || strings.TrimSpace(payload.Model) == "" {
		return "", "model-required"
	}
	if expected == "" {
		return payload.Model, "model-pin-unresolved"
	}
	if payload.Model != expected {
		return payload.Model, "model-mismatch"
	}
	return payload.Model, ""
}
