package server

import (
	"net/http"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

func TestGithubMappingsGatedOnSignIn(t *testing.T) {
	for _, status := range []string{"signed-out", "connected", "revoked", "wrong-identity", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			s := testServer(t)
			if err := s.Store.AddSecret(store.SecretRec{ID: "group", Name: "GitHub", Kind: "github-app", Placeholder: "piso_group"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Store.UpsertGithubApp(store.GithubAppRec{ID: "app", SecretID: "group", AppID: 7, PrivateKey: "pem", Installations: []store.GithubInstallation{{ID: 3, Account: "acme"}}}); err != nil {
				t.Fatal(err)
			}
			if status != "signed-out" {
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
			if status == "connected" {
				if !strings.Contains(w.Body.String(), "acme") || !strings.Contains(w.Body.String(), "piso_group") {
					t.Fatal(w.Body.String())
				}
			} else if strings.TrimSpace(w.Body.String()) != "[]" {
				t.Fatalf("gate leaked mappings: %s", w.Body.String())
			}
			// Management sign-in must never remove worker app credentials.
			if app, ok := s.Store.GithubAppByID("app"); !ok || app.PrivateKey != "pem" {
				t.Fatal("gate changed worker credentials")
			}
		})
	}
}
