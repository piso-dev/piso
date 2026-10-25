package server

import (
	"errors"
	"io"
	"net/http"
	"net/url"

	gtwc "piso/gateway/internal/technocore"
	tc "piso/internal/technocore"
)

// Dashboard-only routes: WorkerHandler and MCP deliberately do not expose them.
func (s *Server) registerTasks(mux *http.ServeMux) {
	csrf := http.NewCrossOriginProtection()
	for _, pattern := range []string{"GET /api/v1/tasks", "POST /api/v1/tasks", "GET /api/v1/tasks/{id}"} {
		mux.Handle(pattern, csrf.Handler(http.HandlerFunc(s.handleTasks)))
	}
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	id, err := tc.Load(tc.Path(s.dataDir()))
	if err != nil || id.Status != tc.StatusActive {
		writeUnpaired(w, "Pair this gateway with Technocore to manage tasks.")
		return
	}
	path := "/api/v1/control/tasks"
	if taskID := r.PathValue("id"); taskID != "" {
		path += "/" + url.PathEscape(taskID)
	}
	query := url.Values{}
	for _, key := range []string{"group", "status", "attention", "project", "repo", "q", "cursor", "limit"} {
		if value := r.URL.Query().Get(key); value != "" {
			query.Set(key, value)
		}
	}
	var body []byte
	if r.Method == http.MethodPost {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
		if err != nil {
			writeJSON(w, 413, map[string]string{"error": "Task request is too large."})
			return
		}
	}
	status, payload, err := gtwc.Call(id, r.Method, path, query, body)
	if err != nil {
		if errors.Is(err, gtwc.ErrUnpaired) {
			writeUnpaired(w, "Gateway is not paired.")
			return
		}
		writeJSON(w, 502, map[string]string{"error": "Technocore is unavailable. Retry with the same task ID."})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
