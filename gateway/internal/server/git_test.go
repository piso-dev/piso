package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"piso/gateway/internal/gitaccess"
	"piso/gateway/internal/store"
)

func saveGitForTest(t *testing.T, s *Server) gitConnection {
	t.Helper()
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/git", map[string]any{
		"name": "GitHub token", "token": "test-real-token", "envKey": "GH_TOKEN", "workers": []string{"demo"}, "confirmHost": true,
		"config": gitaccess.Config{Provider: "github", TokenType: "user", BaseURL: "https://github.com", Hints: map[string]string{"merge": "denied"}},
	})
	if w.Code != 201 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "test-real-token") {
		t.Fatal("token leaked in save response")
	}
	var connection gitConnection
	if err := json.Unmarshal(w.Body.Bytes(), &connection); err != nil {
		t.Fatal(err)
	}
	return connection
}

// Full gateway control route -> provider adapter -> authenticated fake GitHub.
func TestGitGatewayDiscovery(t *testing.T) {
	s := testServer(t)
	calls := 0
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/user/repos" || r.URL.Query().Get("page") != "1" || r.Header.Get("Authorization") != "Bearer test-real-token" {
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"full_name":"acme/example","clone_url":"https://test-real-token@evil.test/stolen"}]`))
	}))
	defer api.Close()
	// Rewrite only the network destination; exercise production URL selection.
	transport := api.Client().Transport
	s.HTTP = &http.Client{Transport: gitTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" {
			t.Fatalf("unexpected host: %s", r.URL.Host)
		}
		clone := r.Clone(r.Context())
		u := *r.URL
		clone.URL = &u
		clone.URL.Host = strings.TrimPrefix(api.URL, "https://")
		return transport.RoundTrip(clone)
	})}
	connection := saveGitForTest(t, s)
	if calls != 0 {
		t.Fatal("saving must not contact GitHub")
	}
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/git/"+connection.ID+"/repositories", map[string]any{})
	if w.Code != 200 {
		t.Fatalf("discovery: %d %s", w.Code, w.Body.String())
	}
	var result gitaccess.Listing
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Repositories) != 1 || result.Repositories[0].CloneURL != "https://github.com/acme/example.git" || result.Repositories[0].ConnectionID != connection.ID {
		t.Fatalf("unexpected listing: %+v", result)
	}
	if strings.Contains(w.Body.String(), "test-real-token") || strings.Contains(w.Body.String(), "evil.test") {
		t.Fatal("untrusted clone URL or credential leaked")
	}
	rec, _ := s.Store.SecretByID(connection.ID)
	if rec.GatewayOnly || !store.SecretVisibleToWorker(rec, "demo") || store.SecretVisibleToWorker(rec, "other") {
		t.Fatal("incorrect worker scope")
	}
	if !strings.Contains(store.RenderPlaceholdersEnvForWorker([]store.SecretRec{rec}, "demo"), "GH_TOKEN=piso_git_") {
		t.Fatal("placeholder missing")
	}
	if len(s.Store.Rules()) != 2 {
		t.Fatal("expected exact github.com and api.github.com bindings")
	}
}

type gitTestTransport func(*http.Request) (*http.Response, error)

func (f gitTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLegacyGitRoutesAreRemoved(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/api/v1/github-app", "/api/v1/github-app/mappings", "/api/v1/github-app/import", "/api/v1/github-user", "/api/v1/github-user/authorize", "/api/v1/github-user/config"} {
		for _, method := range []string{"GET", "POST", "PUT"} {
			w := doJSON(t, s.ControlHandler(), method, path, map[string]any{})
			if w.Code != http.StatusNotFound {
				t.Fatalf("legacy route %s %s still available: %d", method, path, w.Code)
			}
		}
	}
	for _, kind := range []string{"github-app", "github-user", "github-oauth"} {
		payload := map[string]any{"name": "retired", "token": "old-token", "envKey": "GH_TOKEN", "confirmHost": true, "config": map[string]any{"provider": kind, "baseURL": "https://github.com"}}
		w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/git", payload)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("retired provider accepted: %s %d", kind, w.Code)
		}
	}
	if len(s.Store.Secrets()) != 0 || len(s.Store.Rules()) != 0 {
		t.Fatal("rejected legacy requests created credentials")
	}
}

func TestGitValidationAndLifecycle(t *testing.T) {
	s := testServer(t)
	h := s.ControlHandler()
	connection := saveGitForTest(t, s)
	payload := map[string]any{"name": "Gitea", "token": "", "envKey": "GH_TOKEN", "workers": []string{"demo"}, "confirmHost": true, "config": gitaccess.Config{Provider: "gitea", BaseURL: "https://git.example.test", Hints: map[string]string{"read": "denied"}}}
	payload["confirmHost"] = false
	if w := doJSON(t, h, "PUT", "/api/v1/git/"+connection.ID, payload); w.Code != 400 {
		t.Fatalf("unconfirmed host accepted: %d", w.Code)
	}
	payload["confirmHost"] = true
	if w := doJSON(t, h, "PUT", "/api/v1/git/"+connection.ID, payload); w.Code != 200 {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	rec, _ := s.Store.SecretByID(connection.ID)
	if rec.Value != "test-real-token" || rec.Git.Provider != "gitea" || rec.Placeholder != connection.Placeholder {
		t.Fatal("edit must retain token and placeholder")
	}
	rules := s.Store.Rules()
	if len(rules) != 1 || rules[0].Host != "git.example.test" {
		t.Fatalf("old host binding survived: %+v", rules)
	}
	listed := doJSON(t, h, "GET", "/api/v1/git", nil)
	if strings.Contains(listed.Body.String(), "test-real-token") {
		t.Fatal("GET leaked token")
	}
	if w := doJSON(t, h, "DELETE", "/api/v1/git/"+connection.ID, nil); w.Code != 204 {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	if len(s.Store.Rules()) != 0 {
		t.Fatal("remove left substitution rules")
	}
}
