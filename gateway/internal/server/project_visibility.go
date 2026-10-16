package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"piso/gateway/internal/store"
)

func (s *Server) handleDeleteWorker(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !workerNameRe.MatchString(name) {
		writeJSON(w, 400, map[string]string{"error": "worker name required"})
		return
	}
	if err := s.Store.DeleteWorker(name); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleProjectVisibility(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hidden *bool `json:"hidden"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Hidden == nil {
		writeJSON(w, 400, map[string]string{"error": "hidden boolean required"})
		return
	}
	if err := s.Store.SetProjectHidden(r.PathValue("slug"), *in.Hidden); err != nil {
		status := 500
		if errors.Is(err, store.ErrWorkerNotFound) {
			status = 404
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"hidden": *in.Hidden})
}
