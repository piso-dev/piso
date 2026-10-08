package server

import (
	"net/http"
	"sort"
	"strings"

	"piso/gateway/internal/model"
)

// GithubMapping is the credential-free dashboard view of a routing group.
// Never serialize SecretRec or GithubAppRec directly from this endpoint.
type GithubMapping struct {
	SecretID     string            `json:"secretId"`
	Name         string            `json:"name"`
	Placeholder  string            `json:"placeholder"`
	EnvKey       string            `json:"envKey"`
	Workers      []string          `json:"workers"`
	GatewayOnly  bool              `json:"gatewayOnly"`
	AllowedHosts []string          `json:"allowedHosts"`
	Apps         []model.GithubApp `json:"apps"`
	Conflicts    []string          `json:"conflicts"`
	Warnings     []string          `json:"warnings"`
}

func (s *Server) handleGithubMappings(w http.ResponseWriter, r *http.Request) {
	out := []GithubMapping{}
	// The dashboard gates this section on a live GitHub sign-in; the API
	// enforces the same boundary rather than trusting the UI. A revoked or
	// missing token lists nothing (HTTP 200, empty list) so a stale browser
	// cannot scrape mapping data.
	if _, err := s.verifiedGithubUser(r); err != nil {
		writeJSON(w, 200, out)
		return
	}
	apps := s.Store.GithubApps()
	for _, sec := range s.Store.Secrets() {
		if sec.Kind != model.SecretKindGithubApp {
			continue
		}
		group := GithubMapping{SecretID: sec.ID, Name: sec.Name, Placeholder: sec.Placeholder, EnvKey: sec.EnvKey, Workers: sec.Workers, GatewayOnly: sec.GatewayOnly, AllowedHosts: sec.AllowedHosts, Apps: []model.GithubApp{}, Conflicts: []string{}, Warnings: []string{}}
		counts := map[string]int{}
		for _, app := range apps {
			if app.SecretID != sec.ID {
				continue
			}
			app.Placeholder = sec.Placeholder
			group.Apps = append(group.Apps, app.ToSummary())
			if app.RefreshError != "" {
				group.Warnings = append(group.Warnings, app.Name+": "+app.RefreshError)
			}
			if app.PrivateKey == "" || app.AppID == 0 {
				continue
			}
			for _, inst := range app.Installations {
				counts[strings.ToLower(inst.Account)]++
			}
		}
		for account, n := range counts {
			if n > 1 {
				group.Conflicts = append(group.Conflicts, account)
			}
		}
		sort.Strings(group.Conflicts)
		out = append(out, group)
	}
	writeJSON(w, 200, out)
}
