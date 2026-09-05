package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"piso/gateway/internal/store"
)

// workerIn is the host-CLI registration payload at `piso up`.
type workerIn struct {
	Name string   `json:"name"`
	Slug string   `json:"slug"`
	IPs  []string `json:"ips"`
}

// handlePostWorkers upserts the slug↔IP registry entry from the host CLI.
// This is a control-plane (host→gateway) call: the CLI runs on the host and
// resolves the worker's vpc IPs from docker, then posts them so the proxy can
// tag request logs with the worker slug.
func (s *Server) handlePostWorkers(w http.ResponseWriter, r *http.Request) {
	var in workerIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Slug = strings.TrimSpace(in.Slug)
	if !workerNameRe.MatchString(in.Name) {
		writeJSON(w, 400, map[string]string{"error": "worker name required"})
		return
	}
	if in.Slug == "" {
		in.Slug = slugFromWorker(in.Name)
	}
	// validate + normalize IPs (must be literal IPv4/IPv6, not hostnames)
	ips := make([]string, 0, len(in.IPs))
	seen := map[string]bool{}
	for _, raw := range in.IPs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		ip := net.ParseIP(raw)
		if ip == nil {
			writeJSON(w, 400, map[string]string{"error": "invalid ip " + raw})
			return
		}
		// store the raw literal; dedupe case-insensitively (IPv6 hex)
		key := strings.ToLower(raw)
		if seen[key] {
			continue
		}
		seen[key] = true
		ips = append(ips, raw)
	}
	rec, err := s.Store.UpsertWorker(store.WorkerRec{Name: in.Name, Slug: in.Slug, IPs: ips})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, rec)
}

// handleGetWorkers lists the registry (dashboard / CLI / debugging).
func (s *Server) handleGetWorkers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.Store.Workers())
}

// handleSetWorkerInternet flips the internet kill-switch for a worker.
// POST /api/v1/workers/{name}/internet  body: {"disabled": bool}
func (s *Server) handleSetWorkerInternet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !workerNameRe.MatchString(name) {
		writeJSON(w, 400, map[string]string{"error": "worker name required"})
		return
	}
	var in struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	rec, err := s.Store.SetWorkerInternet(name, in.Disabled)
	if err != nil {
		if errors.Is(err, store.ErrWorkerNotFound) {
			writeJSON(w, 404, map[string]string{"error": "worker not found"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, rec)
}