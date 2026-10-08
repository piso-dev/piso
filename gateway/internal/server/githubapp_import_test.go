package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// stubGithubServer wires a Server whose GitHub API traffic is answered by a
// fake api.github.com with exactly two endpoints: GET /app (identity) and
// GET /app/installations (list). Every test asserts on the returned counters.
func stubGithubServer(t *testing.T, appID int64, installations []store.GithubInstallation) (*Server, *githubStubCalls) {
	t.Helper()
	s := testServer(t)
	calls := &githubStubCalls{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app":
			calls.app++
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				return
			}
			writeJSON(w, 200, map[string]any{"id": appID, "name": "existing-app", "slug": "existing-app"})
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
