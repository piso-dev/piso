package server

import (
	"net/http"
	"strconv"

	"piso/gateway/internal/store"
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
		s.githubConnection(),
		s.boostConnection(),
		{ID: "hammerspace", Name: "Hammerspace", Status: "none", Detail: "Not connected"},
	}
}

func (s *Server) githubConnection() connectionView {
	out := connectionView{ID: "github", Name: "GitHub", Status: "none", Detail: "Not connected"}
	apps := s.Store.GithubApps()
	if len(apps) == 0 {
		return out
	}
	status := store.GithubAppStatusNeedsRegister
	ok := 0
	installs := 0
	for _, a := range apps {
		st := a.Status()
		if st == store.GithubAppStatusOK {
			status = store.GithubAppStatusOK
			ok++
		} else if status != store.GithubAppStatusOK && st == store.GithubAppStatusNeedsInstall {
			status = store.GithubAppStatusNeedsInstall
		}
		installs += len(a.Installations)
	}
	out.Status = status
	switch status {
	case store.GithubAppStatusOK:
		out.Detail = "Connected"
		if installs > 0 {
			out.Detail = "Connected · " + strconv.Itoa(installs) + " installation" + plural(installs)
		}
	case store.GithubAppStatusNeedsInstall:
		out.Detail = "App created · install on an org or account"
	default:
		out.Detail = "Finish creating the GitHub App"
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
