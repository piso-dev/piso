package server

import (
	"net/http"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

// The mapping view is built from local state only, so it renders whether or not
// this browser has a GitHub session. Signing in gates the management routes
// (create, import, install, refresh, remove) — those answer 401/409 on their
// own — not the read-only description of what workers can reach.
//
// This is what a peer gateway sees after receiving a shared connection: no
// local app, no local OAuth App, and a routing group whose installations it
// must be able to inspect.
func TestGithubMappingsWithoutGithubSignIn(t *testing.T) {
	for _, status := range []string{"signed-out", "connected", "revoked", "wrong-identity", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			s := testServer(t)
			if err := s.Store.AddSecret(store.SecretRec{ID: "group", Name: "GitHub", Kind: "github-app", Placeholder: "piso_group"}); err != nil {
				t.Fatal(err)
			}
			// The peer-managed shape: registered app, no local sign-in.
			if _, err := s.Store.UpsertGithubApp(store.GithubAppRec{
				ID: "app", SecretID: "group", Source: store.GithubAppSourceTechnocore,
				AppID: 7, Name: "shared", PrivateKey: "pem",
				Installations: []store.GithubInstallation{{ID: 3, Account: "acme", Repos: []string{"acme/private"}}},
			}); err != nil {
				t.Fatal(err)
			}
			if status == "connected" {
				signInGithubUser(t, s)
			}
			if status == "revoked" || status == "wrong-identity" || status == "unavailable" {
				s.HTTP = &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if status == "wrong-identity" {
						w.Write([]byte(`{"id":9,"login":"other"}`))
						return
					}
					if status == "unavailable" {
						w.WriteHeader(503)
						return
					}
					w.WriteHeader(401)
				})}}
			}
			w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-app/mappings", nil)
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			for _, want := range []string{"acme", "piso_group", "acme/private", `"source":"technocore"`} {
				if !strings.Contains(body, want) {
					t.Fatalf("mapping view missing %q: %s", want, body)
				}
			}
			// Visibility never widens to credentials.
			for _, secret := range []string{"pem", "BEGIN RSA", "clientSecret", "webhookSecret"} {
				if strings.Contains(body, secret) {
					t.Fatalf("gate leaked %q: %s", secret, body)
				}
			}
			// Management sign-in must never remove worker app credentials.
			if app, ok := s.Store.GithubAppByID("app"); !ok || app.PrivateKey != "pem" {
				t.Fatal("gate changed worker credentials")
			}
		})
	}
}
