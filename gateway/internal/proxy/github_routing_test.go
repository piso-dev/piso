package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"piso/gateway/internal/githubapp"
	"piso/gateway/internal/store"
)

func TestGithubSharedSecretRouting(t *testing.T) {
	h, st := testHandler(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	sec := store.SecretRec{ID: "s", Name: "GitHub", Kind: "github-app", Placeholder: "piso_github_routing", Workers: []string{"allowed"}, AllowedHosts: githubapp.AllowedHosts}
	if err := st.AddSecret(sec); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncRulesForSecret(sec); err != nil {
		t.Fatal(err)
	}
	for i, account := range []string{"alpha", "beta"} {
		_, err := st.UpsertGithubApp(store.GithubAppRec{ID: account, SecretID: sec.ID, Placeholder: "stale", AppID: int64(i + 1), PrivateKey: private, Installations: []store.GithubInstallation{{ID: int64(i + 10), Account: account}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	mintCalls := 0
	fail := false
	client := githubapp.NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mintCalls++
		if fail {
			return nil, fmt.Errorf("unavailable")
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(`{"token":"test-installation-` + id + `","expires_at":"2099-01-01T00:00:00Z"}`)), Header: make(http.Header)}, nil
	})})
	h.Minter = githubapp.NewPlaceholderMinter(st, client)
	worker := "allowed"
	h.Worker = func(*http.Request) string { return worker }
	forwards := 0
	wantToken := ""
	h.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		forwards++
		if r.Header.Get("Authorization") != "Bearer "+wantToken {
			t.Fatalf("wrong token for %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	request := func(path string, status int) {
		t.Helper()
		before := forwards
		r := httptest.NewRequest("GET", "http://api.github.com"+path, nil)
		r.Header.Set("Authorization", "Bearer "+sec.Placeholder)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s: status %d want %d", path, w.Code, status)
		}
		if status != 200 && forwards != before {
			t.Fatal("failed request forwarded")
		}
	}
	wantToken = "test-installation-10"
	request("/repos/alpha/repo", 200)
	wantToken = "test-installation-11"
	request("/repos/beta/repo", 200)
	request("/installation/repositories", 407)
	request("/repos/unmapped/repo", 407)
	worker = "other"
	request("/repos/alpha/repo", 407)
	worker = "allowed"
	if mintCalls != 2 {
		t.Fatalf("unexpected mint count %d", mintCalls)
	}
	// Replacing credentials invalidates the cached token without an OAuth fallback.
	a, _ := st.GithubAppByID("alpha")
	a.PrivateKey += "\n"
	st.UpsertGithubApp(a)
	fail = true
	request("/repos/alpha/repo", 502)
	// Duplicate accounts are denied by policy, not just by minting.
	b, _ := st.GithubAppByID("beta")
	b.Installations[0].Account = "alpha"
	st.UpsertGithubApp(b)
	request("/repos/alpha/repo", 407)
}
