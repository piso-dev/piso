package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"piso/gateway/internal/store"
)

// WorkerHandler is the namespaced worker API. It is served on a separate
// listener (default :8083) that is not published to the host. Workers must
// not be given ControlHandler.
func (s *Server) WorkerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/worker/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "time": time.Now().UTC()})
	})
	mux.HandleFunc("GET /api/v1/worker/planning", s.handleWorkerGetPlanning)
	mux.HandleFunc("POST /api/v1/worker/planning", s.handleCreateIngress)
	mux.HandleFunc("POST /api/v1/worker/planning/cancel", s.handleCancelIngress)
	mux.HandleFunc("POST /api/v1/worker/context", s.handleWorkerPostContext)
	return mux
}

// handleWorkerPostContext receives the watcher's live context and stores it.
// Worker-supplied and advisory; validated for shape and sanitized (labels end
// up rendered in the dashboard HTML). No semantic enforcement.
func (s *Server) handleWorkerPostContext(w http.ResponseWriter, r *http.Request) {
	var in store.WorkerCtx
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Worker = strings.TrimSpace(in.Worker)
	in.Slug = strings.TrimSpace(in.Slug)
	if !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "worker name required"})
		return
	}
	if in.Slug == "" {
		in.Slug = slugFromWorker(in.Worker)
	}
	if !workerNameRe.MatchString(in.Slug) {
		writeJSON(w, 400, map[string]string{"error": "slug required"})
		return
	}
	// Sanitize label fields for the dashboard (strip control chars, cap).
	in.Folder = sanitizeLabel(in.Folder, 256)
	in.Project = sanitizeLabel(in.Project, 120)
	in.Branch = sanitizeLabel(in.Branch, 120)
	in.Commit = sanitizeLabel(in.Commit, 40)
	in.Model = sanitizeLabel(in.Model, 120)
	if _, err := s.Store.UpsertWorkerCtx(in); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleWorkerGetPlanning(w http.ResponseWriter, r *http.Request) {
	worker := strings.TrimSpace(r.URL.Query().Get("worker"))
	if !workerNameRe.MatchString(worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	pending := s.Store.PendingIngressForWorker(worker)
	if len(pending) > 0 {
		writeJSON(w, 200, viewIngress(pending[0], false))
		return
	}
	slug := slugFromWorker(worker)
	name := planningRouteName(slug)
	if route, ok := s.Store.RouteByName(name); ok && route.Worker == worker {
		writeJSON(w, 200, ingressView{
			Kind: store.IngressKindPlanning, Status: "approved",
			Worker: worker, Slug: slug, Port: route.Port, Name: name,
			URL: ingressPublicURL(name),
		})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "none"})
}

// sanitizeLabel strips control characters (replacing with a space), collapses
// whitespace runs, and caps length so labels can be embedded safely in the
// dashboard HTML and request-log JSONL. Values are advisory.
func sanitizeLabel(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len(s) > max {
		s = s[:max]
	}
	return s
}
