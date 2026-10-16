package server

import (
	"testing"

	"piso/gateway/internal/store"
)

func TestGithubManifestReusesSharedToken(t *testing.T) {
	s := testServer(t)
	signInGithubUser(t, s)
	before := len(s.Store.Secrets())
	for i := 0; i < 2; i++ {
		w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/manifest", map[string]string{"account": "octocat"})
		if w.Code != 200 {
			t.Fatalf("create %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	apps := s.Store.GithubApps()
	secrets := s.Store.Secrets()
	if len(apps) != 2 || len(secrets) != before+1 {
		t.Fatalf("want two apps and one new secret, got %d apps and %d new secrets", len(apps), len(secrets)-before)
	}
	sec, ok := s.Store.SecretByEnvKey("GH_TOKEN")
	if !ok {
		t.Fatal("missing GH_TOKEN")
	}
	for _, app := range apps {
		if app.SecretID != sec.ID || app.Placeholder != sec.Placeholder {
			t.Fatal("apps must share the GH_TOKEN routing group and placeholder")
		}
	}
}

func TestGithubDefaultGroupUsesGH_TOKENWithLegacyGroups(t *testing.T) {
	s := testServer(t)
	shared := store.SecretRec{ID: "shared", Kind: "github-app", EnvKey: "GH_TOKEN", Placeholder: "piso_shared", Workers: []string{"alpha"}}
	legacy := store.SecretRec{ID: "legacy", Kind: "github-app", EnvKey: "GITHUB_APP_OLD", Placeholder: "piso_legacy", Workers: []string{"beta"}}
	for _, sec := range []store.SecretRec{shared, legacy} {
		if err := s.Store.AddSecret(sec); err != nil {
			t.Fatal(err)
		}
	}
	app, created, err := s.newGithubAppInGroup("")
	if err != nil || created || app.SecretID != shared.ID || app.Placeholder != shared.Placeholder {
		t.Fatalf("did not reuse GH_TOKEN: app=%+v created=%v err=%v", app, created, err)
	}
	for _, before := range []store.SecretRec{shared, legacy} {
		after, _ := s.Store.SecretByID(before.ID)
		if after.EnvKey != before.EnvKey || len(after.Workers) != 1 || after.Workers[0] != before.Workers[0] {
			t.Fatal("reusing GH_TOKEN changed existing worker access scopes")
		}
	}
}

func TestGithubFailedImportPreservesEmptySharedGroup(t *testing.T) {
	s, _ := stubGithubServer(t, 42, nil)
	sec := store.SecretRec{ID: "shared", Kind: "github-app", EnvKey: "GH_TOKEN", Placeholder: "piso_shared"}
	if err := s.Store.AddSecret(sec); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import", importBody(t, 999))
	if w.Code != 502 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if _, ok := s.Store.SecretByID(sec.ID); !ok || len(s.Store.Secrets()) != 1 {
		t.Fatal("failed import removed the existing empty shared group")
	}
}

func TestGithubManifestRejectsConflictingToken(t *testing.T) {
	s := testServer(t)
	signInGithubUser(t, s)
	if err := s.Store.AddSecret(store.SecretRec{ID: "pat", EnvKey: "GH_TOKEN", Placeholder: "piso_pat"}); err != nil {
		t.Fatal(err)
	}
	before := len(s.Store.Secrets())
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/manifest", map[string]string{"account": "octocat"})
	if w.Code != 409 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(s.Store.Secrets()) != before || len(s.Store.GithubApps()) != 0 {
		t.Fatal("must not create a fallback environment variable or overwrite GH_TOKEN")
	}
}
