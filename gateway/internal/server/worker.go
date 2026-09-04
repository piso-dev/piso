package server

import (
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
	return mux
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
