package server

import (
	"sort"
	"strings"
	"testing"

	"piso/gateway/internal/store"
	"piso/gateway/internal/technocore"
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

// A connection that arrived from another gateway is editable here. The envelope
// is authoritative and any gateway may publish, so an edit made on this one is
// adopted by the others instead of being reverted on the next sync.
func TestGithubSharedAppIsEditable(t *testing.T) {
	s := testServer(t)
	signInGithubUser(t, s)
	mirrorGroup(t, s)

	// Add an installation.
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/gha_tc_42/installations",
		map[string]any{"id": 8, "account": "acme"})
	if w.Code != 200 {
		t.Fatalf("add installation: status %d: %s", w.Code, w.Body.String())
	}
	app, ok := s.Store.GithubAppByID("gha_tc_42")
	if !ok || len(app.Installations) != 2 {
		t.Fatalf("installation not added: %+v", app)
	}

	// Remove one.
	w = doJSON(t, s.ControlHandler(), "DELETE", "/api/v1/github-app/gha_tc_42/installations/7", nil)
	if w.Code != 200 {
		t.Fatalf("remove installation: status %d: %s", w.Code, w.Body.String())
	}
	app, _ = s.Store.GithubAppByID("gha_tc_42")
	if len(app.Installations) != 1 || app.Installations[0].ID != 8 {
		t.Fatalf("installation not removed: %+v", app.Installations)
	}

	// Remove the app itself; the group goes with the last one.
	w = doJSON(t, s.ControlHandler(), "DELETE", "/api/v1/github-app/gha_tc_42", nil)
	if w.Code != 204 {
		t.Fatalf("remove app: status %d: %s", w.Code, w.Body.String())
	}
	if _, ok := s.Store.GithubAppByID("gha_tc_42"); ok {
		t.Fatal("app survived removal")
	}
}

// The mapping view reports where each app was last configured — provenance for
// the dashboard, not a permission. Deliberately signed out: a gateway holding a
// connection from a peer has no local app and usually no local OAuth App
// either, and this view is what shows the operator that the connection works.
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

// Two gateways editing different parts of one group must not lose each other's
// work. The loser of the version race pulls the winner's snapshot and re-applies
// its own intent on top instead of surfacing a conflict.
func TestGithubEditRebasesOnLostRace(t *testing.T) {
	s := testServer(t)
	signInGithubUser(t, s)
	mirrorGroup(t, s) // app 42, installation 7

	publishes := 0
	s.githubSync = &githubSyncHooks{
		Publish: func(string) error {
			publishes++
			if publishes == 1 {
				return &technocore.StaleError{Detail: "stale content_version"}
			}
			return nil
		},
		Pull: func() error {
			// What the winner published: the same app with installation 9 added.
			app, ok := s.Store.GithubAppByID("gha_tc_42")
			if !ok {
				t.Fatal("app missing before rebase")
			}
			app.Installations = []store.GithubInstallation{{ID: 7, Account: "octocat"}, {ID: 9, Account: "peer"}}
			app.ContentVersion = 6
			_, err := s.Store.UpsertGithubApp(app)
			return err
		},
	}

	// Our change: install on account 8.
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/gha_tc_42/installations",
		map[string]any{"id": 8, "account": "acme"})
	if w.Code != 200 {
		t.Fatalf("rebase failed: status %d: %s", w.Code, w.Body.String())
	}
	if publishes != 2 {
		t.Fatalf("expected a republish after the rebase, got %d publishes", publishes)
	}
	app, _ := s.Store.GithubAppByID("gha_tc_42")
	var ids []int64
	for _, inst := range app.Installations {
		ids = append(ids, inst.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) != 3 || ids[0] != 7 || ids[1] != 8 || ids[2] != 9 {
		t.Fatalf("both changes did not survive: %+v", ids)
	}
}

// A publish that keeps losing the race gives up rather than spinning, and says
// so: the local edit was replaced by the snapshot, so success would be a lie.
func TestGithubEditGivesUpAfterThreeRaces(t *testing.T) {
	s := testServer(t)
	signInGithubUser(t, s)
	mirrorGroup(t, s)

	publishes := 0
	s.githubSync = &githubSyncHooks{
		Publish: func(string) error {
			publishes++
			return &technocore.StaleError{Detail: "stale content_version"}
		},
		Pull: func() error { return nil },
	}
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/gha_tc_42/installations",
		map[string]any{"id": 8, "account": "acme"})
	if w.Code != 502 {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body.String())
	}
	if publishes != githubWriteAttempts {
		t.Fatalf("publishes: %d", publishes)
	}
}
