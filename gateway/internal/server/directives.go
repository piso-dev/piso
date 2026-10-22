package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	gtwc "piso/gateway/internal/technocore"
	tc "piso/internal/technocore"
)

// registerDirectives wires the dashboard's Directives tab. These routes are
// dashboard-only: workers read and propose directives through MCP, and the
// gateway never exposes a worker-facing HTTP copy of this control surface.
func (s *Server) registerDirectives(mux *http.ServeMux) {
	csrf := http.NewCrossOriginProtection()
	for _, pattern := range []string{
		"GET /api/v1/directives",
		"POST /api/v1/directives",
		"PUT /api/v1/directives/{id}",
		"DELETE /api/v1/directives/{id}",
	} {
		mux.Handle(pattern, csrf.Handler(http.HandlerFunc(s.handleDirectives)))
	}
}

func (s *Server) handleDirectives(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	id, err := tc.Load(tc.Path(s.dataDir()))
	if err != nil || id.Status != tc.StatusActive {
		writeJSON(w, 503, map[string]string{"error": "Pair this gateway with Technocore to manage directives."})
		return
	}
	path := "/api/v1/control/directives"
	if directiveID := r.PathValue("id"); directiveID != "" {
		path += "/" + url.PathEscape(directiveID)
	}
	var body []byte
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
		if err != nil {
			writeJSON(w, 413, map[string]string{"error": "Directive request is too large."})
			return
		}
	}
	status, payload, err := gtwc.Call(id, r.Method, path, nil, body)
	if err != nil {
		status = 502
		message := "Technocore is unavailable. Retry with the same directive ID."
		if errors.Is(err, gtwc.ErrUnpaired) {
			status = 503
			message = "Gateway is not paired."
		}
		writeJSON(w, status, map[string]string{"error": message})
		return
	}
	// The dashboard stores a bare array per collection; unwrap the envelope once
	// here instead of teaching every reader about it.
	if r.Method == http.MethodGet && status == http.StatusOK {
		var envelope struct {
			Directives json.RawMessage `json:"directives"`
		}
		if err := json.Unmarshal(payload, &envelope); err == nil {
			if len(envelope.Directives) == 0 {
				envelope.Directives = json.RawMessage("[]")
			}
			payload = envelope.Directives
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
