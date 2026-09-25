package server

import (
	"encoding/json"
	"io"
	"net/http"

	"piso/gateway/internal/technocore"
)

func (s *Server) registerTechnocore(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/technocore", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, technocore.Snapshot(s.dataDir()))
	})
	mux.HandleFunc("POST /api/v1/technocore/share", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name         string   `json:"name"`
			Placeholder  string   `json:"placeholder"`
			EnvKey       string   `json:"envKey"`
			AllowedHosts []string `json:"allowedHosts"`
			Value        string   `json:"value"`
			Recipients   []string `json:"recipients"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "read body"})
			return
		}
		if err := json.Unmarshal(body, &in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if in.Value == "" || in.Name == "" {
			writeJSON(w, 400, map[string]string{"error": "name and value required"})
			return
		}
		out, err := technocore.Share(s.Store, s.dataDir(), technocore.ShareInput{
			Name:         in.Name,
			Placeholder:  in.Placeholder,
			EnvKey:       in.EnvKey,
			AllowedHosts: in.AllowedHosts,
			Plaintext:    in.Value,
			Recipients:   in.Recipients,
		})
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, out)
	})
	mux.HandleFunc("POST /api/v1/technocore/publish", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Placeholder string   `json:"placeholder"`
			Recipients  []string `json:"recipients"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if in.Placeholder == "" {
			writeJSON(w, 400, map[string]string{"error": "placeholder required"})
			return
		}
		out, err := technocore.PublishPlaceholder(s.Store, s.dataDir(), in.Placeholder, in.Recipients)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, out)
	})
}

func (s *Server) dataDir() string {
	if s.DataDir != "" {
		return s.DataDir
	}
	return ".piso"
}
