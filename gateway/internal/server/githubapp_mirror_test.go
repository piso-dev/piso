package server

import (
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

// mirrorGroup seeds a routing group holding one app configured on another
// gateway, as technocore would have imported it.
func mirrorGroup(t *testing.T, s *Server) {
	t.Helper()
	if err := s.Store.AddSecret(store.SecretRec{
		ID: "tc_rid", Name: "GitHub", Kind: "github-app",
		Placeholder: "piso_github_peer", EnvKey: "GH_TOKEN", AllowedHosts: []string{"api.github.com"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.UpsertGithubApp(store.GithubAppRec{
		ID: "gha_tc_42", SecretID: "tc_rid", Source: store.GithubAppSourceTechnocore,
		TechnocoreID: "rid", AppID: 42, Name: "piso-peer", Slug: "piso-peer",
		PrivateKey:    "-----BEGIN RSA PRIVATE KEY-----\nkey\n-----END RSA PRIVATE KEY-----",
		Installations: []store.GithubInstallation{{ID: 7, Account: "octocat"}},
	}); err != nil {
		t.Fatal(err)
	}
}

// Every mutating GitHub route must refuse a mirrored app: the envelope would
// overwrite the edit on the next sync, so the dashboard sends the operator to
// the gateway that owns it.
func TestGithubMirroredAppIsReadOnly(t *testing.T) {
	s := testServer(t)
	signInGithubUser(t, s)
	mirrorGroup(t, s)
	stub := testGithubAppPEM(t)

	cases := []struct {
		name, method, path string
		body               any
	}{
		{"import", "POST", "/api/v1/github-app/import", map[string]any{"appId": 42, "privateKey": stub.PrivateKey, "secretId": "tc_rid"}},
		{"manifest", "POST", "/api/v1/github-app/manifest", map[string]any{"secretId": "tc_rid", "account": "octocat"}},
		{"add installation", "POST", "/api/v1/github-app/gha_tc_42/installations", map[string]any{"id": 8, "account": "acme"}},
		{"remove installation", "DELETE", "/api/v1/github-app/gha_tc_42/installations/7", nil},
		{"refresh", "POST", "/api/v1/github-app/gha_tc_42/refresh", nil},
		{"remove app", "DELETE", "/api/v1/github-app/gha_tc_42", nil},
	}
	for _, c := range cases {
		w := doJSON(t, s.ControlHandler(), c.method, c.path, c.body)
		if w.Code != 409 {
			t.Fatalf("%s: status %d: %s", c.name, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "another gateway") {
			t.Fatalf("%s: unhelpful error: %s", c.name, w.Body.String())
		}
	}

	// Nothing above changed the mirror.
	app, ok := s.Store.GithubAppByID("gha_tc_42")
	if !ok || len(app.Installations) != 1 || app.Installations[0].ID != 7 {
		t.Fatalf("mirror mutated: %+v", app)
	}
}

// The mapping view tells the dashboard which apps it may not edit.
// Deliberately signed out: a gateway holding a connection from a peer has no
// local app and usually no local OAuth App either, and this view is what tells
// the operator (and the dashboard's read-only rows) that the connection is
// there. Signing in gates actions, not visibility.
func TestGithubMappingsReportAppSource(t *testing.T) {
	s := testServer(t)
	mirrorGroup(t, s)
	if _, err := s.Store.UpsertGithubApp(store.GithubAppRec{
		ID: "gha_local", SecretID: "tc_rid", Source: store.GithubAppSourceLocal,
		AppID: 43, Name: "local", PrivateKey: "pem",
		Installations: []store.GithubInstallation{{ID: 9, Account: "acme"}},
	}); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-app/mappings", nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"source":"technocore"`) || !strings.Contains(body, `"source":"local"`) {
		t.Fatalf("sources missing: %s", body)
	}
	if strings.Contains(body, "BEGIN RSA PRIVATE KEY") {
		t.Fatal("PEM leaked into the mapping view")
	}
}
