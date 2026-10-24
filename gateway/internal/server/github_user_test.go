package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

// A client-secret rotation on the same OAuth App must not sign the user out.
// The previous handler rebuilt the whole record and dropped the token, so
// re-saving the config looked exactly like a logout.
func TestGithubUserConfigKeepsSession(t *testing.T) {
	s := testServer(t)
	if err := s.Store.AddSecret(store.SecretRec{ID: "sec_cs", Name: "GitHub OAuth client secret", Kind: "github-oauth", Placeholder: "piso_cs", Value: "old-client-secret", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AddSecret(store.SecretRec{ID: "sec_tok", Name: "GitHub user OAuth — gateway only", Kind: "github-oauth", Placeholder: "piso_tok", Value: "user-token", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetGithubUser(store.GithubUserRec{ClientID: "client-id", ClientSecretID: "sec_cs", TokenSecretID: "sec_tok", Login: "octocat", AccountID: 583231}); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.ControlHandler(), "PUT", "/api/v1/github-user/config", map[string]string{"clientId": "client-id", "clientSecret": "new-client-secret"})
	if w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	u := s.Store.GithubUser()
	if u.Login != "octocat" || u.AccountID != 583231 || u.TokenSecretID != "sec_tok" {
		t.Fatalf("config save signed the user out: %+v", u)
	}
	if sec, ok := s.Store.SecretByID("sec_cs"); !ok || sec.Value != "new-client-secret" {
		t.Fatalf("client secret not replaced in place: %+v", sec)
	}
	if n := countNamedSecret(s, "GitHub OAuth client secret"); n != 1 {
		t.Fatalf("expected one client secret, got %d", n)
	}
}

// A different OAuth App invalidates the token minted by the old one.
func TestGithubUserConfigClearsTokenOnNewApp(t *testing.T) {
	s := testServer(t)
	if err := s.Store.AddSecret(store.SecretRec{ID: "sec_cs", Name: "GitHub OAuth client secret", Kind: "github-oauth", Placeholder: "piso_cs", Value: "old-client-secret", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AddSecret(store.SecretRec{ID: "sec_tok", Name: "GitHub user OAuth — gateway only", Kind: "github-oauth", Placeholder: "piso_tok", Value: "user-token", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetGithubUser(store.GithubUserRec{ClientID: "client-id", ClientSecretID: "sec_cs", TokenSecretID: "sec_tok", Login: "octocat", AccountID: 583231}); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.ControlHandler(), "PUT", "/api/v1/github-user/config", map[string]string{"clientId": "other-id", "clientSecret": "new-client-secret"})
	if w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	u := s.Store.GithubUser()
	if u.ClientID != "other-id" || u.TokenSecretID != "" || u.Login != "" || u.AccountID != 0 {
		t.Fatalf("new app kept the old token: %+v", u)
	}
	if _, ok := s.Store.SecretByID("sec_tok"); ok {
		t.Fatal("old token secret was not removed")
	}
}

func countNamedSecret(s *Server, name string) int {
	n := 0
	for _, rec := range s.Store.Secrets() {
		if rec.Name == name {
			n++
		}
	}
	return n
}

// Re-authenticating must replace the one token row, not append another.
func TestSaveGithubUserSecretUpserts(t *testing.T) {
	s := testServer(t)
	first, err := s.saveGithubUserSecret("GitHub user OAuth — gateway only", "token-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.saveGithubUserSecret("GitHub user OAuth — gateway only", "token-2")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("re-auth created a new row: %s -> %s", first, second)
	}
	if n := countNamedSecret(s, "GitHub user OAuth — gateway only"); n != 1 {
		t.Fatalf("expected one token secret, got %d", n)
	}
	if sec, ok := s.Store.SecretByID(first); !ok || sec.Value != "token-2" {
		t.Fatalf("value not replaced: %+v", sec)
	}
}

// A recorded identity whose token secret was deleted is a genuine sign-out:
// the existing status contract (200/connected:false) still holds. The fixes
// above are what keep the token from being deleted in the first place.
func TestGithubUserStatusSignedOutWhenNoIdentity(t *testing.T) {
	s := testServer(t)
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-user", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var out struct {
		Connected bool `json:"connected"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Connected {
		t.Fatal("empty record reported connected")
	}
}

// Re-authenticating must rotate the token in place. The callback used to save
// a new row and then delete the old one by id; once saveGithubUserSecret
// upserts, that id is the row just written, so the delete must be skipped.
func TestGithubUserCallbackKeepsRotatedToken(t *testing.T) {
	s := testServer(t)
	if err := s.Store.AddSecret(store.SecretRec{ID: "sec_cs", Name: "GitHub OAuth client secret", Kind: "github-oauth", Placeholder: "piso_cs", Value: "client-secret", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AddSecret(store.SecretRec{ID: "sec_tok", Name: "GitHub user OAuth — gateway only", Kind: "github-oauth", Placeholder: "piso_tok", Value: "old-token", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetGithubUser(store.GithubUserRec{ClientID: "client-id", ClientSecretID: "sec_cs", TokenSecretID: "sec_tok", Login: "octocat", AccountID: 583231}); err != nil {
		t.Fatal(err)
	}
	s.HTTP = &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Host == "github.com" && r.URL.Path == "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"new-token","scope":"read:user read:org"}`))
		case r.URL.Host == "api.github.com" && r.URL.Path == "/user":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":583231,"login":"octocat","type":"User"}`))
		default:
			w.WriteHeader(404)
		}
	})}}
	s.githubOAuthMu.Lock()
	s.githubOAuthStates = map[string]githubOAuthAttempt{
		"state-1": {verifier: "verifier-1", browser: "browser-1", clientID: "client-id", expires: time.Now().Add(time.Minute)},
	}
	s.githubOAuthMu.Unlock()
	req := httptest.NewRequest("GET", githubUserCallback()+"?code=one-time&state=state-1", nil)
	req.AddCookie(&http.Cookie{Name: "piso_github_oauth", Value: "browser-1"})
	w := httptest.NewRecorder()
	s.WebHandler().ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	u := s.Store.GithubUser()
	if u.TokenSecretID == "" {
		t.Fatal("callback cleared the token reference")
	}
	if sec, ok := s.Store.SecretByID(u.TokenSecretID); !ok || sec.Value != "new-token" {
		t.Fatalf("rotated token not stored: ok=%v rec=%+v", ok, sec)
	}
	if n := countNamedSecret(s, "GitHub user OAuth — gateway only"); n != 1 {
		t.Fatalf("expected one token secret, got %d", n)
	}
}

func TestGithubScopeGranted(t *testing.T) {
	for _, tc := range []struct {
		granted string
		want    bool
	}{
		{"read:user read:org", true},
		{"read:user,read:org", true},
		{"read:org", true},
		{"read:user", false},
		{"", false},
		{"read:user read:orgs", false},
	} {
		if got := githubScopeGranted(tc.granted, "read:org"); got != tc.want {
			t.Fatalf("githubScopeGranted(%q) = %v, want %v", tc.granted, got, tc.want)
		}
	}
}

// A valid token without read:org still identifies the person. It must sign in
// and record the gap, not reject the login with a dead-end error.
func TestGithubUserCallbackSignsInWithoutOrgScope(t *testing.T) {
	s := testServer(t)
	if err := s.Store.AddSecret(store.SecretRec{ID: "sec_cs", Name: "GitHub OAuth client secret", Kind: "github-oauth", Placeholder: "piso_cs", Value: "client-secret", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetGithubUser(store.GithubUserRec{ClientID: "client-id", ClientSecretID: "sec_cs"}); err != nil {
		t.Fatal(err)
	}
	s.HTTP = &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Host == "github.com" && r.URL.Path == "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"new-token","scope":"read:user"}`))
		case r.URL.Host == "api.github.com" && r.URL.Path == "/user":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":583231,"login":"octocat","type":"User"}`))
		default:
			w.WriteHeader(404)
		}
	})}}
	s.githubOAuthMu.Lock()
	s.githubOAuthStates = map[string]githubOAuthAttempt{
		"state-1": {verifier: "verifier-1", browser: "browser-1", clientID: "client-id", expires: time.Now().Add(time.Minute)},
	}
	s.githubOAuthMu.Unlock()
	req := httptest.NewRequest("GET", githubUserCallback()+"?code=one-time&state=state-1", nil)
	req.AddCookie(&http.Cookie{Name: "piso_github_oauth", Value: "browser-1"})
	w := httptest.NewRecorder()
	s.WebHandler().ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("sign-in without read:org must still succeed: %d %s", w.Code, w.Body.String())
	}
	u := s.Store.GithubUser()
	if u.Login != "octocat" || githubScopeGranted(u.Scopes, "read:org") {
		t.Fatalf("recorded grant: %+v", u)
	}
	status := doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-user", nil)
	var out struct {
		Connected bool  `json:"connected"`
		OrgAccess *bool `json:"orgAccess"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Connected || out.OrgAccess == nil || *out.OrgAccess {
		t.Fatalf("status must report the missing org scope: %s", status.Body.String())
	}
	orgs := doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-user/organizations", nil)
	if orgs.Code != 401 || !strings.Contains(orgs.Body.String(), "read:org") {
		t.Fatalf("org discovery error must be actionable: %d %s", orgs.Code, orgs.Body.String())
	}
}
