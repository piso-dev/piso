package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"piso/gateway/internal/store"
)

func TestFailedImportPreservesExistingGroup(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		s, _ := stubGithubServer(t, 42, nil)
		if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import", importBody(t, 42)); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		before := s.Store.GithubApps()[0]
		body := importBody(t, 999)
		if explicit {
			body["secretId"] = before.SecretID
		}
		w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import", body)
		if w.Code != 502 {
			t.Fatalf("status %d", w.Code)
		}
		if len(s.Store.GithubApps()) != 1 || len(s.Store.Secrets()) != 1 {
			t.Fatal("failed import deleted shared credentials")
		}
		after, ok := s.Store.GithubAppByID(before.ID)
		if !ok || after.PrivateKey != before.PrivateKey || after.SecretID != before.SecretID {
			t.Fatal("existing app changed")
		}
	}
}

func TestGithubInstallCallbackState(t *testing.T) {
	s := testServer(t)
	sec := store.SecretRec{ID: "group", Kind: "github-app", Placeholder: "piso_group"}
	if err := s.Store.AddSecret(sec); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		rec := testGithubAppPEM(t)
		rec.ID = id
		rec.SecretID = sec.ID
		rec.Slug = "app-" + id
		if id == "b" {
			rec.AppID = 43
		}
		if _, err := s.Store.UpsertGithubApp(rec); err != nil {
			t.Fatal(err)
		}
	}
	fetches := 0
	s.HTTP = &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/9" {
			fetches++
			issuer, err := jwtIssuer(r.Header.Get("Authorization"))
			if err != nil || issuer != 43 {
				t.Fatalf("callback used wrong app: %d %v", issuer, err)
			}
			io.WriteString(w, `{"id":9,"account":{"login":"beta","type":"Organization"}}`)
			return
		}
		w.WriteHeader(503) // Repo-list failure must not lose the validated installation.
	})}}
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-app/b/install", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var out map[string]string
	json.Unmarshal(w.Body.Bytes(), &out)
	address, err := url.Parse(out["url"])
	if err != nil {
		t.Fatal(err)
	}
	state := address.Query().Get("state")
	if state == "" || state == "app-b" {
		t.Fatal("state must be unpredictable, not a slug")
	}
	callback := func(state string, want int) {
		t.Helper()
		r := httptest.NewRequest("GET", "/github-app/setup?installation_id=9&state="+url.QueryEscape(state), nil)
		w := httptest.NewRecorder()
		s.WebHandler().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("callback status %d want %d: %s", w.Code, want, w.Body.String())
		}
	}
	callback("", 400)
	callback("app-b", 400)
	callback(state, 302)
	callback(state, 400)
	a, _ := s.Store.GithubAppByID("a")
	b, _ := s.Store.GithubAppByID("b")
	if len(a.Installations) != 0 || len(b.Installations) != 1 || b.Installations[0].Account != "beta" || fetches != 1 {
		t.Fatal("callback bound incorrectly or replayed")
	}
	b.InstallState = "expired"
	b.StateExpires = time.Now().Add(-time.Minute)
	s.Store.UpsertGithubApp(b)
	callback("expired", 400)
	if fetches != 1 {
		t.Fatal("expired callback reached GitHub")
	}
}

func TestGithubRefreshRediscoversAndRemovalPreservesGroup(t *testing.T) {
	s, _ := stubGithubServer(t, 42, []store.GithubInstallation{{ID: 9, Account: "new-account"}})
	sec := store.SecretRec{ID: "group", Kind: "github-app", Placeholder: "piso_group"}
	if err := s.Store.AddSecret(sec); err != nil {
		t.Fatal(err)
	}
	rec := testGithubAppPEM(t)
	rec.ID = "app"
	rec.SecretID = sec.ID
	rec.Installations = []store.GithubInstallation{{ID: 8, Account: "old-account"}}
	if _, err := s.Store.UpsertGithubApp(rec); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/app/refresh", map[string]any{})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	fresh, _ := s.Store.GithubAppByID("app")
	if len(fresh.Installations) != 1 || fresh.Installations[0].Account != "new-account" {
		t.Fatalf("stale discovery: %+v", fresh.Installations)
	}
	if !strings.Contains(fresh.RefreshError, "incomplete") {
		t.Fatal("repository-list failure must be visible")
	}
	rec.ID = "other"
	s.Store.UpsertGithubApp(rec)
	w = doJSON(t, s.ControlHandler(), "DELETE", "/api/v1/github-app/app", nil)
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	if _, ok := s.Store.SecretByID(sec.ID); !ok || len(s.Store.GithubApps()) != 1 {
		t.Fatal("removing one app deleted the shared group")
	}
}

func TestGithubManifestDoesNotOverwriteConnectedApp(t *testing.T) {
	s, _ := stubGithubServer(t, 42, nil)
	if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import", importBody(t, 42)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	app := s.Store.GithubApps()[0]
	signInGithubUser(t, s)
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/manifest", map[string]string{"secretId": app.SecretID, "account": "octocat"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	after, _ := s.Store.GithubAppByID(app.ID)
	if len(s.Store.GithubApps()) != 2 || after.PrivateKey != app.PrivateKey || after.ManifestState != "" {
		t.Fatal("manifest overwrote existing app")
	}
}
