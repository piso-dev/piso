package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

// signInGithubUser stores a connected GitHub user whose live identity
// check is answered by the in-process transport.
func signInGithubUser(t *testing.T, s *Server) {
	t.Helper()
	if err := s.Store.AddSecret(store.SecretRec{ID: "sec_user", Name: "user token", Kind: "github-oauth", Placeholder: "piso_user", Value: "user-token-value", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetGithubUser(store.GithubUserRec{TokenSecretID: "sec_user", Login: "octocat", AccountID: 583231}); err != nil {
		t.Fatal(err)
	}
	s.HTTP = &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" && r.Header.Get("Authorization") == "Bearer user-token-value" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":583231,"login":"octocat","type":"User"}`))
			return
		}
		w.WriteHeader(401)
	})}}
}

// Exercise the registered HTTP route, not the handler directly: two apps
// share one restricted secret, with an independent gateway-only group.
func TestGithubMappingsHTTP(t *testing.T) {
	s := testServer(t)
	signInGithubUser(t, s)
	for _, sec := range []store.SecretRec{
		{ID: "shared", Name: "Shared GitHub", Kind: "github-app", Placeholder: "piso_shared", EnvKey: "GH_TOKEN", Workers: []string{"alpha"}, AllowedHosts: []string{"api.github.com"}},
		{ID: "private", Name: "Gateway only", Kind: "github-app", Placeholder: "piso_private", GatewayOnly: true},
		{ID: "oauth", Kind: "github-oauth", Placeholder: "piso_oauth", Value: "oauth-value-must-not-leak", GatewayOnly: true},
	} {
		if err := s.Store.AddSecret(sec); err != nil {
			t.Fatal(err)
		}
	}
	for _, app := range []store.GithubAppRec{
		{ID: "app-a", SecretID: "shared", Name: "App A", AppID: 10, PrivateKey: "pem-must-not-leak", ClientSecret: "client-secret-must-not-leak", InstallState: "nonce-must-not-leak", Installations: []store.GithubInstallation{{ID: 101, Account: "Acme", Repos: []string{"Acme/repo"}}}},
		{ID: "app-b", SecretID: "shared", Name: "App B", AppID: 20, PrivateKey: "another-pem", Installations: []store.GithubInstallation{{ID: 202, Account: "acme"}}},
		{ID: "app-c", SecretID: "private", Name: "App C"},
	} {
		if _, err := s.Store.UpsertGithubApp(app); err != nil {
			t.Fatal(err)
		}
	}
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-app/mappings", nil)
	if w.Code != 200 {
		t.Fatalf("route status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("not JSON: %s", w.Header())
	}
	// Decode independently of GithubMapping so incorrect JSON field names fail.
	var groups []struct {
		SecretID     string   `json:"secretId"`
		Placeholder  string   `json:"placeholder"`
		EnvKey       string   `json:"envKey"`
		Workers      []string `json:"workers"`
		GatewayOnly  bool     `json:"gatewayOnly"`
		AllowedHosts []string `json:"allowedHosts"`
		Apps         []struct {
			ID            string `json:"id"`
			Status        string `json:"status"`
			Installations []struct {
				ID      int64    `json:"id"`
				Account string   `json:"account"`
				Repos   []string `json:"repos"`
			} `json:"installations"`
		} `json:"apps"`
		Conflicts []string `json:"conflicts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("expected two github groups, got %s", w.Body.String())
	}
	shared := groups[0]
	if shared.SecretID != "shared" || shared.Placeholder != "piso_shared" || shared.EnvKey != "GH_TOKEN" || len(shared.Workers) != 1 || shared.Workers[0] != "alpha" || shared.GatewayOnly || len(shared.AllowedHosts) != 1 || shared.AllowedHosts[0] != "api.github.com" {
		t.Fatalf("incorrect secret scope: %+v", shared)
	}
	if len(shared.Apps) != 2 || shared.Apps[0].ID != "app-a" || shared.Apps[1].ID != "app-b" {
		t.Fatalf("incorrect grouping: %+v", shared.Apps)
	}
	if shared.Apps[0].Status != "ok" || len(shared.Apps[0].Installations) != 1 || shared.Apps[0].Installations[0].ID != 101 || shared.Apps[0].Installations[0].Account != "Acme" || len(shared.Apps[0].Installations[0].Repos) != 1 {
		t.Fatalf("incorrect mapping: %+v", shared.Apps[0])
	}
	if len(shared.Conflicts) != 1 || shared.Conflicts[0] != "acme" {
		t.Fatalf("missing case-insensitive conflict: %+v", shared.Conflicts)
	}
	if groups[1].SecretID != "private" || !groups[1].GatewayOnly || len(groups[1].Apps) != 1 || groups[1].Apps[0].Status != "needs-register" {
		t.Fatalf("gateway-only group: %+v", groups[1])
	}
	for _, forbidden := range []string{"pem-must-not-leak", "another-pem", "client-secret-must-not-leak", "nonce-must-not-leak", "oauth-value-must-not-leak", `"privateKey"`, `"clientSecret"`, `"installState"`, `"value"`} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("response exposed credential field or value %q", forbidden)
		}
	}
}
