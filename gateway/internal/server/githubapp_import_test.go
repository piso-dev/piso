package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

// handlerTransport serves HTTP requests in-process: no network, no egress
// proxy. It lets tests stub api.github.com without env-dependent behavior.
type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	res := rec.Result()
	res.Request = req
	return res, nil
}

// githubStubCalls counts hits per fake endpoint.
type githubStubCalls struct{ app, installations, other int }

// jwtIssuer extracts the unsigned iss claim from an Authorization header.
// Signature verification is the gateway's job; the stub only checks that
// the JWT names the app being called.
func jwtIssuer(auth string) (int64, error) {
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
	if len(parts) != 3 {
		return 0, fmt.Errorf("not a jwt")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, err
	}
	var claim struct {
		Iss int64 `json:"iss"`
	}
	if err := json.Unmarshal(raw, &claim); err != nil {
		return 0, err
	}
	return claim.Iss, nil
}

// stubGithubApps wires a Server whose GitHub API traffic is answered by a
// fake api.github.com. apps is the registry of apps that "exist on GitHub"
// (app ID → slug): /app returns the identity of the JWT issuer only when it
// is registered, mirroring GitHub's signature-verified lookup — an app ID
// whose key GitHub does not know yields 401. Every test asserts on the
// returned counters.
func stubGithubApps(t *testing.T, apps map[int64]string, installations []store.GithubInstallation) (*Server, *githubStubCalls) {
	t.Helper()
	s := testServer(t)
	calls := &githubStubCalls{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app":
			calls.app++
			iss, err := jwtIssuer(r.Header.Get("Authorization"))
			if err != nil {
				w.WriteHeader(401)
				return
			}
			slug, ok := apps[iss]
			if !ok {
				w.WriteHeader(401)
				return
			}
			writeJSON(w, 200, map[string]any{"id": iss, "name": "existing-app", "slug": slug})
		case "/app/installations":
			calls.installations++
			out := []map[string]any{}
			for _, inst := range installations {
				out = append(out, map[string]any{
					"id":       inst.ID,
					"html_url": inst.HTMLURL,
					"account":  map[string]string{"login": inst.Account, "type": inst.AccountType},
				})
			}
			writeJSON(w, 200, out)
		default:
			calls.other++
			w.WriteHeader(404)
		}
	})
	s.HTTP = &http.Client{Transport: handlerTransport{handler: handler}}
	return s, calls
}

// stubGithubServer registers a single app in the fake GitHub registry.
func stubGithubServer(t *testing.T, appID int64, installations []store.GithubInstallation) (*Server, *githubStubCalls) {
	return stubGithubApps(t, map[int64]string{appID: "existing-app"}, installations)
}

func importBody(t *testing.T, appID int64) map[string]any {
	t.Helper()
	return map[string]any{"appId": appID, "privateKey": testGithubAppPEM(t).PrivateKey}
}

func TestImportGithubAppVerifiesBeforeSaving(t *testing.T) {
	s, calls := stubGithubServer(t, 42, []store.GithubInstallation{
		{ID: 9, Account: "complex-gh", AccountType: "User"},
		{ID: 11, Account: "acme-corp", AccountType: "Organization"},
	})
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import", importBody(t, 42))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var out store.GithubAppRec
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Slug != "existing-app" || out.AppID != 42 {
		t.Fatalf("%+v", out)
	}
	if len(out.Installations) != 2 ||
		out.Installations[0].Account != "complex-gh" ||
		out.Installations[1].Account != "acme-corp" ||
		out.Installations[1].AccountType != "Organization" {
		t.Fatalf("installations %+v", out.Installations)
	}
	if len(out.Installations[0].Repos) != 0 {
		t.Fatalf("repos should be empty until refresh: %+v", out.Installations[0])
	}
	if calls.app != 1 || calls.installations != 1 || calls.other != 0 {
		t.Fatalf("expected /app and /app/installations once each, got %+v", calls)
	}
}

func TestImportGithubAppRejectsWrongAppID(t *testing.T) {
	s, calls := stubGithubServer(t, 42, nil)
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import", importBody(t, 999))
	if w.Code != 502 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if apps := s.Store.GithubApps(); len(apps) != 0 {
		t.Fatalf("nothing should be saved on failure, got %+v", apps)
	}
	if calls.app != 1 || calls.installations != 0 || calls.other != 0 {
		t.Fatalf("wrong ID must fail on /app before listing, got %+v", calls)
	}
}

func TestImportGithubAppLargeAppIDOK(t *testing.T) {
	s, _ := stubGithubServer(t, 4_826_315, []store.GithubInstallation{
		{ID: 9, Account: "complex-gh", AccountType: "User"},
	})
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import", importBody(t, 4_826_315))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestImportGithubAppRejectsBadPEM(t *testing.T) {
	s, calls := stubGithubServer(t, 42, nil)
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import",
		map[string]any{"appId": 42, "privateKey": "not a pem"})
	if w.Code != 400 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if calls.app != 0 || calls.installations != 0 || calls.other != 0 {
		t.Fatalf("bad PEM must fail before any GitHub call, got %+v", calls)
	}
}

func TestImportGithubAppRequiresBothFields(t *testing.T) {
	s, calls := stubGithubServer(t, 42, nil)
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import",
		map[string]any{"appId": 42})
	if w.Code != 400 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if calls.app != 0 || calls.installations != 0 || calls.other != 0 {
		t.Fatalf("missing fields must fail before any GitHub call, got %+v", calls)
	}
}

func TestImportGithubAppAppendsToGroup(t *testing.T) {
	s, calls := stubGithubApps(t, map[int64]string{42: "personal-app", 43: "org-app"}, []store.GithubInstallation{{ID: 9, Account: "alpha", AccountType: "User"}})
	if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import", importBody(t, 42)); w.Code != 200 {
		t.Fatalf("first import: %d %s", w.Code, w.Body.String())
	}
	// Second import with a different app ID lands in the same group and
	// does not replace the first app.
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import", importBody(t, 43))
	if w.Code != 200 {
		t.Fatalf("second import: %d %s", w.Code, w.Body.String())
	}
	apps := s.Store.GithubApps()
	if len(apps) != 2 {
		t.Fatalf("want 2 apps, got %+v", apps)
	}
	if apps[0].SecretID != apps[1].SecretID {
		t.Fatal("apps must share one routing group secret")
	}
	if apps[0].Placeholder != apps[1].Placeholder || apps[0].Placeholder == "" {
		t.Fatal("apps must share one placeholder")
	}
	if sec, ok := s.Store.SecretByID(apps[0].SecretID); !ok || sec.Kind != "github-app" || sec.EnvKey != "GH_TOKEN" {
		t.Fatalf("group secret %+v", sec)
	}
	if calls.app != 2 || calls.installations != 2 {
		t.Fatalf("expected /app + /app/installations per import, got %+v", calls)
	}
	// Same app ID again is a reconnect, not a third app.
	w = doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import", importBody(t, 42))
	if w.Code != 200 {
		t.Fatalf("reconnect: %d %s", w.Code, w.Body.String())
	}
	if apps = s.Store.GithubApps(); len(apps) != 2 {
		t.Fatalf("reconnect must not add an app, got %d", len(apps))
	}
}

func TestImportGithubAppUnknownGroup404(t *testing.T) {
	s, calls := stubGithubServer(t, 42, nil)
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import",
		map[string]any{"appId": 42, "privateKey": testGithubAppPEM(t).PrivateKey, "secretId": "sec_nope"})
	if w.Code != 404 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if calls.app != 0 || len(s.Store.GithubApps()) != 0 {
		t.Fatal("unknown group must fail before any GitHub call")
	}
}

func TestImportGithubAppFailedImportLeavesNoPlaceholder(t *testing.T) {
	s, _ := stubGithubServer(t, 42, nil)
	// Wrong app ID: Discover fails; the auto-created group secret must be
	// removed so a failed import leaves no GH_TOKEN placeholder behind.
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/import", importBody(t, 999))
	if w.Code != 502 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(s.Store.GithubApps()) != 0 || len(s.Store.Secrets()) != 0 {
		t.Fatalf("failed import must clean up, apps=%d secrets=%d", len(s.Store.GithubApps()), len(s.Store.Secrets()))
	}
}
