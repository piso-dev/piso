package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	gtwc "piso/gateway/internal/technocore"
	tc "piso/internal/technocore"
)

func (s *Server) coordinatorJSON(method, path string, body []byte) (int, []byte, error) {
	id, err := tc.Load(tc.Path(s.dataDir()))
	if err != nil {
		return 0, nil, gtwc.ErrUnpaired
	}
	return gtwc.Call(id, method, path, nil, body)
}

// unpairedCode marks a refusal that is a state, not a failure: this gateway has
// no active coordinator link. The dashboard reads it as "not available yet" and
// renders the pairing banner instead of a refresh error, so an unpaired install
// does not look broken in the browser console.
const unpairedCode = "technocore_unpaired"

func writeUnpaired(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": message, "code": unpairedCode})
}

func (s *Server) handleAsksList(w http.ResponseWriter, r *http.Request) {
	status, payload, err := s.coordinatorJSON(http.MethodGet, "/api/v1/asks", nil)
	if err != nil {
		writeJSON(w, 200, map[string]any{"asks": []any{}})
		return
	}
	if status >= 400 {
		writeJSON(w, 200, map[string]any{"asks": []any{}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	if len(payload) == 0 {
		_, _ = w.Write([]byte(`{"asks":[]}`))
		return
	}
	_, _ = w.Write(payload)
}

func (s *Server) handleAskApprove(w http.ResponseWriter, r *http.Request) {
	s.proxyAskWrite(w, r, http.MethodPost, "/api/v1/approvals/"+r.PathValue("id")+"/decide")
}

func (s *Server) handleAskResolve(w http.ResponseWriter, r *http.Request) {
	s.proxyAskWrite(w, r, http.MethodPost, "/api/v1/human-actions/"+r.PathValue("id")+"/resolve")
}

// handleTaskRespond forwards a human review/guidance/cancel decision on a task
// wait. The coordinator keeps the wait and task transition atomic.
func (s *Server) handleTaskRespond(w http.ResponseWriter, r *http.Request) {
	s.proxyAskWrite(w, r, http.MethodPost, "/api/v1/tasks/"+r.PathValue("id")+"/human-respond")
}

func (s *Server) proxyAskWrite(w http.ResponseWriter, r *http.Request, method, path string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad body"})
		return
	}
	status, payload, err := s.coordinatorJSON(method, path, body)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "unpaired"})
		return
	}
	if status == 0 {
		status = 502
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if len(payload) == 0 && status >= 400 {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": strings.TrimSpace(http.StatusText(status))})
		return
	}
	_, _ = w.Write(payload)
}
