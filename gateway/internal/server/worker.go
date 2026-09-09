package server

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
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
	mux.HandleFunc("POST /api/v1/worker/ports", s.handleWorkerPostPorts)
	mux.HandleFunc("POST /api/v1/worker/activity", s.handleWorkerPostActivity)
	mux.HandleFunc("GET /api/v1/worker/activities", s.handleWorkerGetActivities)
	mux.HandleFunc("POST /api/v1/worker/activity/revoke", s.handleWorkerRevokeActivity)
	return mux
}

// portIn is one listener reported by the ports watcher.
type portIn struct {
	Port      int    `json:"port"`
	Reachable bool   `json:"reachable,omitempty"`
	Note      string `json:"note,omitempty"`
}

// portsIn is the watcher's full-set report: identity + every listening TCP
// port with reachability from the vpc.
type portsIn struct {
	Worker string   `json:"worker"`
	Slug   string   `json:"slug,omitempty"`
	Ports  []portIn `json:"ports"`
}

// handleWorkerPostPorts receives the ports watcher's listener set and
// auto-creates <slug>-<port> routes for the reachable ports (the expose loop
// in the plan: any server, any worker, no host ceremony). Origin marks routes
// as watcher-managed (rate-capped, GC'd after down-grace) vs host-created
// (sticky). Identity is checked against the slug↔IP registry so a worker
// cannot squat another worker's label namespace.
func (s *Server) handleWorkerPostPorts(w http.ResponseWriter, r *http.Request) {
	var in portsIn
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
	// A registered worker's source IP must match the reported identity.
	if reg, ok := s.Store.WorkerByIP(requestIP(r)); ok && reg.Name != in.Worker {
		writeJSON(w, 403, map[string]string{"error": "identity mismatch"})
		return
	}
	ports := make([]int, 0, len(in.Ports))
	unreachable := make([]store.UnreachablePort, 0, len(in.Ports))
	seen := map[int]bool{}
	for _, p := range in.Ports {
		if p.Port < 1 || p.Port > 65535 || seen[p.Port] {
			continue
		}
		seen[p.Port] = true
		if p.Reachable {
			ports = append(ports, p.Port)
			continue
		}
		unreachable = append(unreachable, store.UnreachablePort{Port: p.Port, Note: sanitizeLabel(p.Note, 80)})
	}
	if _, err := s.Store.SyncWorkerPorts(in.Worker, in.Slug, ports); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.Store.SetWorkerUnreachable(in.Worker, unreachable)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// requestIP returns the caller's host IP (no port) for registry lookups.
func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host
}

// activityIn is one informant report (or monitor poke) posted by a worker.
type activityIn struct {
	Worker     string `json:"worker"`
	Slug       string `json:"slug,omitempty"`
	Kind       string `json:"kind"`
	TargetSlug string `json:"targetSlug,omitempty"`
	Text       string `json:"text"`
}

// handleWorkerPostActivity stores an activity reported by a worker's
// informant (or the monitor). The origin worker is identity-checked against
// the slug↔IP registry exactly like the ports handler; text is sanitized
// (labels render in the host dashboard). No headers/bodies ever enter the
// activity store — only the sanitized text.
func (s *Server) handleWorkerPostActivity(w http.ResponseWriter, r *http.Request) {
	var in activityIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Worker = strings.TrimSpace(in.Worker)
	in.Slug = strings.TrimSpace(in.Slug)
	in.Kind = strings.TrimSpace(in.Kind)
	in.TargetSlug = strings.TrimSpace(in.TargetSlug)
	in.Text = strings.TrimSpace(in.Text)
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
	if !store.ValidActivityKind(in.Kind) {
		writeJSON(w, 400, map[string]string{"error": "unknown kind"})
		return
	}
	if in.Text == "" {
		writeJSON(w, 400, map[string]string{"error": "text required"})
		return
	}
	// A registered worker's source IP must match the reported identity.
	if reg, ok := s.Store.WorkerByIP(requestIP(r)); ok && (reg.Name != in.Worker || reg.Slug != in.Slug) {
		writeJSON(w, 403, map[string]string{"error": "identity mismatch"})
		return
	}
	a := store.Activity{
		Worker: in.Worker, Slug: in.Slug, Kind: in.Kind,
		TargetSlug: sanitizeLabel(in.TargetSlug, 63),
		Text:       sanitizeLabel(in.Text, 500),
	}
	if err := s.Store.InsertActivity(a); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, a)
}

// handleWorkerGetActivities returns the activity feed. The monitor polls this;
// any registered worker may read the whole feed (it is scrubbed: sanitized
// text only). Caller identity is verified like the ports handler.
func (s *Server) handleWorkerGetActivities(w http.ResponseWriter, r *http.Request) {
	worker := strings.TrimSpace(r.URL.Query().Get("worker"))
	if !workerNameRe.MatchString(worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	if reg, ok := s.Store.WorkerByIP(requestIP(r)); ok && reg.Name != worker {
		writeJSON(w, 403, map[string]string{"error": "identity mismatch"})
		return
	}
	f := store.ActivityFilter{
		Kind:    strings.TrimSpace(r.URL.Query().Get("kind")),
		Slug:    strings.TrimSpace(r.URL.Query().Get("slug")),
		Limit:   atoiDefault(r.URL.Query().Get("limit"), 500),
	}
	acts, err := s.Store.QueryActivities(f)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if acts == nil {
		acts = []store.Activity{}
	}
	writeJSON(w, 200, acts)
}

// handleWorkerRevokeActivity removes one of the caller's own activity rows
// (id is scoped to the posting worker).
func (s *Server) handleWorkerRevokeActivity(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Worker string `json:"worker"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.ID = strings.TrimSpace(in.ID)
	in.Worker = strings.TrimSpace(in.Worker)
	if in.ID == "" || !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "id and worker required"})
		return
	}
	if reg, ok := s.Store.WorkerByIP(requestIP(r)); ok && reg.Name != in.Worker {
		writeJSON(w, 403, map[string]string{"error": "identity mismatch"})
		return
	}
	deleted, err := s.Store.DeleteOwnActivity(in.ID, in.Worker)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if !deleted {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	w.WriteHeader(204)
}

// atoiDefault parses s as an int, returning def on empty/invalid.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return def
	}
	return n
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
