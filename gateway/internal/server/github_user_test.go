package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

// stubGithubUserServer wires a Server with a signed-in GitHub user and a
// fake api.github.com that answers membership discovery only for the
// stored OAuth token. Requests are delivered on a channel so tests can
// assert on what actually left the gateway.
func stubGithubUserServer(t *testing.T) (*Server, <-chan *http.Request) {
	t.Helper()
	s := testServer(t)
	if err := s.Store.AddSecret(store.SecretRec{ID: "sec_cs", Name: "client secret", Kind: "github-oauth", Placeholder: "piso_cs", Value: "client-secret-value", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AddSecret(store.SecretRec{ID: "sec_tok", Name: "user token", Kind: "github-oauth", Placeholder: "piso_tok", Value: "user-token-value", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetGithubUser(store.GithubUserRec{ClientID: "client-id", ClientSecretID: "sec_cs", TokenSecretID: "sec_tok", Login: "octocat", AccountID: 583231}); err != nil {
		t.Fatal(err)
	}
	requests := make(chan *http.Request, 16)
	s.HTTP = &http.Client{Transport: stubGithubUserTransport{requests: requests, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-token-value" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/user":
			w.Write([]byte(`{"id":583231,"login":"octocat","type":"User"}`))
		case r.URL.Path == "/user/memberships/orgs":
			w.Write([]byte(`[
				{"state":"active","role":"admin","organization":{"id":1,"login":"acme","type":"Organization"}},
				{"state":"active","role":"member","organization":{"id":2,"login":"members-only"}},
				{"state":"pending","role":"admin","organization":{"id":3,"login":"pending-org"}},
				{"state":"active","role":"admin","organization":{"id":4,"login":"Acme"}}
			]`))
		default:
			w.WriteHeader(404)
		}
	})}}
	return s, requests
}

// stubGithubUserTransport records requests on a channel before serving.
type stubGithubUserTransport struct {
	handler  http.Handler
	requests chan *http.Request
}

func (t stubGithubUserTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.requests <- r
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, r)
	res := rec.Result()
	res.Request = r
	return res, nil
}

func TestGithubUserOrganizations(t *testing.T) {
	s, requests := stubGithubUserServer(t)
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-user/organizations", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var ctx struct {
		Login     string `json:"login"`
		AccountID int64  `json:"accountId"`
		Orgs      []struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"orgs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Login != "octocat" || ctx.AccountID != 583231 {
		t.Fatalf("identity %+v", ctx)
	}
	// Only active admin memberships are offered for private-app creation.
	if len(ctx.Orgs) != 2 || ctx.Orgs[0].ID != 1 || ctx.Orgs[0].Login != "acme" || ctx.Orgs[0].Type != "Organization" || ctx.Orgs[1].ID != 4 {
		t.Fatalf("org filtering wrong: %+v", ctx.Orgs)
	}
	if seen := <-requests; seen.URL.Path != "/user" {
		t.Fatal("must verify live identity first")
	}
	select {
	case seen := <-requests:
		if seen == nil || !strings.Contains(seen.URL.Path, "/user/memberships") || !strings.Contains(seen.URL.RawQuery, "state=active") {
			t.Fatal("discovery must call the active memberships endpoint")
		}
	default:
		t.Fatal("discovery must call the active memberships endpoint")
	}
}

func TestGithubUserAuthorizeNavigation(t *testing.T) {
	s, _ := stubGithubUserServer(t)
	r := httptest.NewRequest("GET", githubAppPublicBase()+"/api/v1/github-user/authorize", nil)
	w := httptest.NewRecorder()
	s.WebHandler().ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	next, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := next.Query()
	if next.Host != "github.com" || next.Path != "/login/oauth/authorize" || q.Get("redirect_uri") != githubUserCallback() || q.Get("client_id") != "client-id" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("incorrect OAuth redirect: %s", next)
	}
	cookies := w.Result().Cookies()
	attempt, ok := s.githubOAuthStates[q.Get("state")]
	if !ok || len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].Value != attempt.browser || len(q.Get("state")) < 32 {
		t.Fatal("missing unpredictable state or browser binding")
	}
	// A callback without the binding cookie must not consume the attempt.
	bad := httptest.NewRecorder()
	s.WebHandler().ServeHTTP(bad, httptest.NewRequest("GET", githubUserCallback()+"?code=fake&state="+q.Get("state"), nil))
	if bad.Code != 400 || len(s.githubOAuthStates) != 1 {
		t.Fatal("unbound callback accepted")
	}
}

func TestGithubManifestAccountSelection(t *testing.T) {
	for _, tc := range []struct {
		account, path string
		code          int
	}{
		{"octocat", "/settings/apps/new", 200},
		{"ACME", "/organizations/acme/settings/apps/new", 200},
		{"members-only", "", 403},
		{"pending-org", "", 403},
		{"unowned", "", 403},
		{"", "", 403},
	} {
		t.Run(tc.account, func(t *testing.T) {
			s, _ := stubGithubUserServer(t)
			w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/manifest", map[string]string{"account": tc.account})
			if w.Code != tc.code {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if tc.code != 200 {
				if len(s.Store.GithubApps()) != 0 {
					t.Fatal("rejected owner created an app")
				}
				return
			}
			var out struct {
				Action   string
				Manifest struct{ Public bool }
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(out.Action)
			if u.Host != "github.com" || u.Path != tc.path || u.Query().Get("state") == "" || out.Manifest.Public {
				t.Fatalf("bad private-app registration: %s", w.Body.String())
			}
		})
	}
}

func TestGithubUserOrganizationsRequiresSignIn(t *testing.T) {
	s := testServer(t)
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-user/organizations", nil)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "sign in") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
