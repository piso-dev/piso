package server

import (
	"net/http"
	"strconv"

	"piso/gateway/internal/gitaccess"
)

type connectionView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"connections": s.connectionViews()})
}

func (s *Server) connectionViews() []connectionView {
	return []connectionView{
		s.gitConnection(),
		s.boostConnection(),
		{ID: "hammerspace", Name: "Hammerspace", Status: "none", Detail: "Not connected"},
	}
}

func (s *Server) gitConnection() connectionView {
	out := connectionView{ID: "git", Name: "Git", Status: "none", Detail: "Not connected"}
	n := 0
	for _, rec := range s.Store.Secrets() {
		if rec.Kind == gitaccess.Kind && rec.Git != nil && !rec.Revoked {
			n++
		}
	}
	if n > 0 {
		out.Status = "ok"
		out.Detail = strconv.Itoa(n) + " token connection" + plural(n)
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
