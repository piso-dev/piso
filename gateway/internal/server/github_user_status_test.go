package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

type githubStatusTransport func(*http.Request) (*http.Response, error)

func (f githubStatusTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGithubUserStatusVerification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantCode  int
		connected bool
	}{
		{"valid", 200, `{"id":583231,"login":"octocat"}`, 200, true},
		{"revoked", 401, `{"message":"Bad credentials"}`, 200, false},
		{"identity changed", 200, `{"id":123,"login":"someone-else"}`, 200, false},
		{"forbidden or rate limited", 403, `{}`, 503, false},
		{"rate limited", 429, `{}`, 503, false},
		{"upstream failure", 502, `{}`, 503, false},
		{"malformed response", 200, `{`, 503, false},
		{"incomplete identity", 200, `{}`, 503, false},
		{"network failure", 0, ``, 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := stubGithubUserServer(t)
			workingHTTP := s.HTTP
			if tc.status == 0 {
				s.HTTP = &http.Client{Transport: githubStatusTransport(func(*http.Request) (*http.Response, error) {
					return nil, fmt.Errorf("network unavailable")
				})}
			} else {
				s.HTTP = &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					w.Write([]byte(tc.body))
				})}}
			}
			before := s.Store.GithubUser()
			w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-user", nil)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantCode, w.Body.String())
			}
			var out map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if tc.wantCode == 200 {
				if out["connected"] != tc.connected || out["configured"] != true {
					t.Fatalf("unexpected status: %s", w.Body.String())
				}
			} else if _, ok := out["connected"]; ok || out["error"] == nil {
				t.Fatalf("unavailable verification must not report disconnected: %s", w.Body.String())
			}
			if s.Store.GithubUser() != before {
				t.Fatal("status check modified persisted connection")
			}
			if _, ok := s.githubUserToken(); !ok {
				t.Fatal("status check removed stored token")
			}
			if !tc.connected {
				w = doJSON(t, s.ControlHandler(), "POST", "/api/v1/github-app/manifest", map[string]string{"account": "octocat"})
				if w.Code < 400 || len(s.Store.GithubApps()) != 0 {
					t.Fatal("failed live verification must still block app creation")
				}
			}
			// No reauthorization is needed once verification succeeds again.
			s.HTTP = workingHTTP
			w = doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-user", nil)
			var recovered struct{ Connected bool }
			if err := json.Unmarshal(w.Body.Bytes(), &recovered); err != nil || w.Code != 200 || !recovered.Connected {
				t.Fatalf("connection did not recover: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestGithubUserStatusWithoutToken(t *testing.T) {
	s, _ := stubGithubUserServer(t)
	if err := s.Store.DeleteSecret("sec_tok"); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-user", nil)
	var out struct{ Connected, Configured bool }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || out.Connected || !out.Configured {
		t.Fatalf("missing token must require sign-in: %d %s", w.Code, w.Body.String())
	}
}
