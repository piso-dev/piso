package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Reason codes for a refused model pin. They are the proxy's error contract:
// each one lands in X-Piso-Reason, in the request log, and starts the 403 body,
// so a caller can tell "the request went to the wrong host" from "this worker
// may not use that model" without reading the gateway's state.
const (
	pinReasonRequired    = "model-required"
	pinReasonUnresolved  = "model-pin-unresolved"
	pinReasonProvider    = "model-provider-mismatch"
	pinReasonNotApproved = "model-not-allowed"
)

// pinVerdict is the model-pin answer for one request. Model is the id the
// caller asked for and is recorded even when the request is refused, so the
// log answers "what did it try to run" and not just "it was blocked".
type pinVerdict struct {
	Model  string
	Reason string
	// Message is the human-readable 403 body: it names the requested model and
	// the ids that would have been accepted, because a bare reason code is not
	// enough to fix a pin.
	Message string
	// Allowed is the set the request was judged against. It is empty when the
	// pin could not resolve a provider at all.
	Allowed []string
}

// Refused reports whether the pin rejects this request.
func (v pinVerdict) Refused() bool { return v.Reason != "" }

// Header is the response header set for a refused pin. X-Piso-Reason is always
// present so an existing caller keeps working; the allowed list is added
// whenever the pin got far enough to resolve one.
func (v pinVerdict) Header() http.Header {
	h := http.Header{"X-Piso-Reason": {v.Reason}}
	if len(v.Allowed) > 0 {
		h.Set("X-Piso-Allowed-Models", strings.Join(v.Allowed, ","))
	}
	return h
}

// modelPin restricts generation requests to the model set of the provider the
// worker is pinned to; it never overrides the scanner or domain policy.
// Catalogs and unrelated traffic pass untouched.
//
// The host half of the check is a boundary, not a preference: the provider's
// secret is substituted as a literal value on that host, so a request to any
// other host cannot be served and must not be forwarded. The model half is a
// policy — which ids the provider's account may use — so it is judged against
// the provider's approved list rather than against one string.
func (h *Handler) modelPin(req *http.Request, body []byte) pinVerdict {
	if req.Method != http.MethodPost {
		return pinVerdict{}
	}
	if !h.isManagedGeneration(req) {
		return pinVerdict{}
	}
	selected := h.Store.PiProvider()
	if id := h.Store.ModelOverrideProviderID(h.workerID(req)); id != "" {
		selected = h.Store.PiProviderByID(id)
	}
	if selected == nil {
		return pinVerdict{
			Reason:  pinReasonUnresolved,
			Message: pinReasonUnresolved + ": no model provider is configured for this gateway; add one in the dashboard",
		}
	}
	base, err := url.Parse(selected.BaseURL)
	if err != nil || !strings.EqualFold(base.Host, req.URL.Host) || base.Scheme != req.URL.Scheme || !strings.HasPrefix(req.URL.Path, strings.TrimSuffix(base.Path, "/")+"/") {
		return pinVerdict{
			Reason: pinReasonProvider,
			Message: fmt.Sprintf(
				"%s: this worker's model requests go to %s (provider %q); this request went to %s://%s%s",
				pinReasonProvider, selected.BaseURL, selected.ID, req.URL.Scheme, req.URL.Host, req.URL.Path),
		}
	}

	var payload struct {
		Model string `json:"model"`
	}
	requested := ""
	if json.Unmarshal(body, &payload) == nil {
		requested = strings.TrimSpace(payload.Model)
	}
	verdict := pinVerdict{Model: requested, Allowed: selected.AllowedModelIDs()}
	if requested == "" {
		verdict.Reason = pinReasonRequired
		verdict.Message = fmt.Sprintf(
			"%s: the request body names no model; provider %q accepts %s",
			pinReasonRequired, selected.ID, modelListText(verdict.Allowed))
		return verdict
	}
	expected := selected.DefaultModelID()
	if expected == "" || len(verdict.Allowed) == 0 {
		verdict.Reason = pinReasonUnresolved
		verdict.Message = fmt.Sprintf(
			"%s: provider %q has no default model and no catalog, so there is nothing to pin to; save a default model in the dashboard",
			pinReasonUnresolved, selected.ID)
		return verdict
	}
	if !containsModel(verdict.Allowed, requested) {
		verdict.Reason = pinReasonNotApproved
		verdict.Message = fmt.Sprintf(
			"%s: this worker may not use %q; provider %q approves %s",
			pinReasonNotApproved, requested, selected.ID, modelListText(verdict.Allowed))
		return verdict
	}
	return verdict
}

// isManagedGeneration reports whether req targets a generation endpoint of any
// provider in the dashboard stack. Requests to hosts no profile owns are left
// to the domain policy: they are not this pin's business.
func (h *Handler) isManagedGeneration(req *http.Request) bool {
	for _, provider := range h.Store.PiProviderStack() {
		base, err := url.Parse(provider.BaseURL)
		if err != nil || !strings.EqualFold(base.Host, req.URL.Host) || base.Scheme != req.URL.Scheme {
			continue
		}
		prefix := strings.TrimSuffix(base.Path, "/")
		path := strings.TrimSuffix(req.URL.Path, "/")
		for _, suffix := range []string{"/chat/completions", "/responses", "/messages", "/completions"} {
			if path == prefix+suffix {
				return true
			}
		}
	}
	return false
}

func containsModel(allowed []string, want string) bool {
	for _, id := range allowed {
		if id == want {
			return true
		}
	}
	return false
}

// modelListText renders an allowed set for an error body without letting a
// large catalog turn one 403 into a page.
func modelListText(allowed []string) string {
	const limit = 8
	switch {
	case len(allowed) == 0:
		return "no models"
	case len(allowed) == 1:
		return fmt.Sprintf("%q", allowed[0])
	case len(allowed) <= limit:
		quoted := make([]string, 0, len(allowed))
		for _, id := range allowed {
			quoted = append(quoted, fmt.Sprintf("%q", id))
		}
		return strings.Join(quoted, ", ")
	default:
		quoted := make([]string, 0, limit)
		for _, id := range allowed[:limit] {
			quoted = append(quoted, fmt.Sprintf("%q", id))
		}
		return fmt.Sprintf("%s and %d more", strings.Join(quoted, ", "), len(allowed)-limit)
	}
}
