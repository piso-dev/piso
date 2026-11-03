package mcp

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"piso/gateway/internal/gitaccess"
	"piso/gateway/internal/store"
)

type gitTransport func(*http.Request) (*http.Response, error)

func (f gitTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestGitUserRepositoriesWorkerScope(t *testing.T) {
	st := activityStore(t)
	for _, row := range []struct {
		id          string
		workers     []string
		gatewayOnly bool
	}{{"visible", []string{"demo"}, false}, {"hidden", []string{"other"}, false}, {"gateway", nil, true}} {
		c := gitaccess.Config{Provider: "gitea", BaseURL: "https://git.test", Hints: map[string]string{"read": "denied"}}
		if err := st.AddSecret(store.SecretRec{ID: row.id, Kind: gitaccess.Kind, Git: &c, Value: row.id, Placeholder: "piso_git_" + row.id, EnvKey: row.id, Workers: row.workers, GatewayOnly: row.gatewayOnly}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	h := New(st, nil, t.TempDir())
	h.GitHTTPClient = &http.Client{Transport: gitTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer visible" {
			t.Fatal("out-of-scope credential used")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[{"full_name":"org/repo"}]`)), Header: http.Header{}}, nil
	})}
	result, err := h.toolUserRepositories(store.WorkerRec{Slug: "demo"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := result.(gitaccess.Listing)
	if calls != 1 || !out.Complete || len(out.Repositories) != 1 || len(out.Hints) != 1 {
		t.Fatalf("scope or advisory hint gated access: %+v", out)
	}
	if _, err := h.toolUserRepositories(store.WorkerRec{Slug: "demo", InternetDisabled: true}, nil); err == nil {
		t.Fatal("offline worker allowed")
	}
	if calls != 1 {
		t.Fatal("offline worker contacted host")
	}
}
