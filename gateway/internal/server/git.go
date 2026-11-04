package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"piso/gateway/internal/gitaccess"
	"piso/gateway/internal/store"
)

type gitConnection struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Config      gitaccess.Config `json:"config"`
	EnvKey      string           `json:"envKey"`
	Placeholder string           `json:"placeholder"`
	Workers     []string         `json:"workers"`
}

func gitView(rec store.SecretRec) gitConnection {
	return gitConnection{rec.ID, rec.Name, *rec.Git, rec.EnvKey, rec.Placeholder, rec.Workers}
}

func (s *Server) registerGit(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/git", func(w http.ResponseWriter, r *http.Request) {
		out := []gitConnection{}
		for _, rec := range s.Store.Secrets() {
			if rec.Kind == gitaccess.Kind && rec.Git != nil && !rec.Revoked {
				out = append(out, gitView(rec))
			}
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /api/v1/git/{id}/repositories", func(w http.ResponseWriter, r *http.Request) {
		rec, ok := s.Store.SecretByID(r.PathValue("id"))
		if !ok || rec.Kind != gitaccess.Kind || rec.Git == nil || rec.Revoked {
			writeJSON(w, 404, map[string]string{"error": "Git connection not found"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		writeJSON(w, 200, gitaccess.List(ctx, s.httpClient(), *rec.Git, rec.Value, rec.ID))
	})
	mux.HandleFunc("POST /api/v1/git", s.handleSaveGit)
	mux.HandleFunc("PUT /api/v1/git/{id}", s.handleSaveGit)
	mux.HandleFunc("DELETE /api/v1/git/{id}", func(w http.ResponseWriter, r *http.Request) {
		rec, ok := s.Store.SecretByID(r.PathValue("id"))
		if !ok || rec.Kind != gitaccess.Kind {
			writeJSON(w, 404, map[string]string{"error": "Git connection not found"})
			return
		}
		if err := s.revokeOrDelete(rec); err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not remove Git connection"})
			return
		}
		w.WriteHeader(204)
	})
}

func (s *Server) handleSaveGit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string           `json:"name"`
		Token       string           `json:"token"`
		Config      gitaccess.Config `json:"config"`
		EnvKey      string           `json:"envKey"`
		Workers     []string         `json:"workers"`
		ConfirmHost bool             `json:"confirmHost"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid Git connection"})
		return
	}
	config, hosts, err := in.Config.Normalize()
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !in.ConfirmHost {
		writeJSON(w, 400, map[string]string{"error": "confirm the credential destinations before saving"})
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	// Tokens are stored verbatim; never mint, normalize, or elevate them.
	if in.Name == "" || !store.ValidEnvKey(in.EnvKey) || strings.ContainsAny(in.Token, "\r\n\x00") {
		writeJSON(w, 400, map[string]string{"error": "name, valid environment key, and single-line token required"})
		return
	}
	rec := store.SecretRec{}
	editing := r.Method == "PUT"
	if editing {
		var ok bool
		rec, ok = s.Store.SecretByID(r.PathValue("id"))
		if !ok || rec.Kind != gitaccess.Kind || rec.Revoked {
			writeJSON(w, 404, map[string]string{"error": "Git connection not found"})
			return
		}
	} else {
		rec.ID = "sec_" + randID()
		rec.Placeholder = "piso_git_" + randID()
		rec.CreatedAt = time.Now().UTC()
	}
	if in.Token != "" {
		rec.Value = in.Token
	}
	if rec.Value == "" {
		writeJSON(w, 400, map[string]string{"error": "token required"})
		return
	}
	// Env keys must be unambiguous even when multiple credentials share a host.
	for _, other := range s.Store.Secrets() {
		if other.ID != rec.ID && !other.Revoked && other.EnvKey == in.EnvKey {
			writeJSON(w, 409, map[string]string{"error": "environment key already belongs to another secret; choose a unique key"})
			return
		}
	}
	rec.Name, rec.Kind, rec.Git = in.Name, gitaccess.Kind, &config
	rec.EnvKey, rec.AllowedHosts, rec.Workers, rec.GatewayOnly = in.EnvKey, hosts, in.Workers, false
	if editing {
		err = s.Store.ReplaceSecret(rec)
		if err == nil {
			_, err = s.seedSecretRules(r.Context(), rec)
		}
	} else {
		_, err = s.saveSecret(r.Context(), rec)
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not save Git connection"})
		return
	}
	status := 201
	if editing {
		status = 200
	}
	writeJSON(w, status, gitView(rec))
}
