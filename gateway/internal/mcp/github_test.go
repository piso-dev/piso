package mcp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"piso/gateway/internal/githubapp"
	"piso/gateway/internal/store"
)

// Exercise tools/call -> scope filter -> real GitHub client -> paginated API.
func TestGithubRepositoriesMCP(t *testing.T) {
	st := activityStore(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	for _, fixture := range []struct {
		id            string
		workers       []string
		gatewayOnly   bool
		installations []store.GithubInstallation
	}{
		{"visible", []string{"demo"}, false, []store.GithubInstallation{{ID: 11, Account: "alpha"}, {ID: 12, Account: "beta"}}},
		{"shared", nil, false, []store.GithubInstallation{{ID: 13, Account: "shared"}}},
		{"hidden", []string{"other"}, false, []store.GithubInstallation{{ID: 14, Account: "hidden"}}},
		{"gateway-only", nil, true, []store.GithubInstallation{{ID: 15, Account: "private"}}},
		{"not-installed", nil, false, nil},
	} {
		if err := st.AddSecret(store.SecretRec{ID: fixture.id, Kind: "github-app", EnvKey: "GH_" + fixture.id, Placeholder: "piso_" + fixture.id, Workers: fixture.workers, GatewayOnly: fixture.gatewayOnly}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertGithubApp(store.GithubAppRec{ID: fixture.id, SecretID: fixture.id, AppID: 1, PrivateKey: privateKey, Installations: fixture.installations}); err != nil {
			t.Fatal(err)
		}
	}
	pages := map[string]int{}
	mints := 0
	failShared := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var id int
			_, _ = fmt.Sscanf(r.URL.Path, "/app/installations/%d/access_tokens", &id)
			if id < 11 || id > 13 {
				t.Errorf("attempt to mint invisible installation: %s", r.URL.Path)
				http.Error(w, "forbidden", 403)
				return
			}
			mints++
			fmt.Fprintf(w, `{"token":"fixture-%d","expires_at":"2099-01-01T00:00:00Z"}`, id)
			return
		}
		if r.URL.Path != "/installation/repositories" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected request %s", r.URL)
			http.Error(w, "unexpected", 400)
			return
		}
		token := r.Header.Get("Authorization")
		pages[token]++
		page := r.URL.Query().Get("page")
		var names []string
		switch token {
		case "Bearer fixture-11":
			switch page {
			case "1":
				// A full page must cause exactly one more request.
				for i := 0; i < 100; i++ {
					names = append(names, "alpha/repo")
				}
			case "2":
				names = []string{"alpha/last"}
			default:
				t.Errorf("pagination did not terminate: %s", page)
				http.Error(w, "too many pages", 400)
				return
			}
		case "Bearer fixture-12":
			names = []string{"beta/repo", "alpha/repo"}
		case "Bearer fixture-13":
			if failShared {
				http.Error(w, "secret upstream response fixture-13", 403)
				return
			}
			// Empty installations must terminate on the first page.
		default:
			t.Errorf("unexpected token %q", token)
		}
		repos := []map[string]string{}
		for _, name := range names {
			repos = append(repos, map[string]string{"full_name": name})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"repositories": repos})
	}))
	defer api.Close()
	client := githubapp.NewClient(api.Client())
	client.BaseURL = api.URL
	h := New(st, nil, t.TempDir())
	h.GithubRepos = client.ListReposContext
	listed := false
	for _, tool := range h.toolList() {
		if tool["name"] == "github_repositories" {
			listed = true
		}
	}
	if !listed {
		t.Fatal("tool not registered")
	}
	caller := store.WorkerRec{Name: "piso-worker-demo", Slug: "demo"}
	call := func() githubRepositoriesResult {
		t.Helper()
		resp := h.callTool(caller, rpcRequest{Params: json.RawMessage(`{"name":"github_repositories","arguments":{}}`)})
		if resp.Error != nil {
			t.Fatal(resp.Error.Message)
		}
		raw, err := json.Marshal(resp.Result)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Content) != 1 {
			t.Fatalf("bad MCP result: %s", raw)
		}
		var out githubRepositoriesResult
		if err := json.Unmarshal([]byte(result.Content[0].Text), &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "fixture-") || strings.Contains(string(raw), "PRIVATE KEY") {
			t.Fatalf("credential leaked: %s", raw)
		}
		return out
	}
	out := call()
	want := []string{"https://github.com/alpha/last", "https://github.com/alpha/repo", "https://github.com/beta/repo"}
	if !out.Complete || len(out.Errors) != 0 || !reflect.DeepEqual(out.Repositories, want) {
		t.Fatalf("result: %+v", out)
	}
	if mints != 3 || !reflect.DeepEqual(pages, map[string]int{"Bearer fixture-11": 2, "Bearer fixture-12": 1, "Bearer fixture-13": 1}) {
		t.Fatalf("mints=%d pages=%v", mints, pages)
	}
	failShared = true
	out = call()
	if out.Complete || len(out.Errors) != 1 || out.Errors[0].AppID != "shared" || !reflect.DeepEqual(out.Repositories, want) {
		t.Fatalf("partial result: %+v", out)
	}
	for _, args := range []string{`{"worker":"other"}`, `{"workerId":"other"}`, `{"slug":"other"}`, `[]`} {
		if _, err := h.toolGithubRepositories(caller, json.RawMessage(args)); err == nil {
			t.Fatalf("accepted %s", args)
		}
	}
	caller.InternetDisabled = true
	if _, err := h.toolGithubRepositories(caller, nil); err == nil {
		t.Fatal("accepted offline worker")
	}
}

func TestGithubRepositoriesEmpty(t *testing.T) {
	h := New(activityStore(t), nil, t.TempDir())
	h.GithubRepos = githubapp.NewClient(nil).ListReposContext
	out, err := h.toolGithubRepositories(store.WorkerRec{Slug: "demo"}, nil)
	if err != nil {
		t.Fatal(err.Message)
	}
	raw, _ := json.Marshal(out)
	if string(raw) != `{"repositories":[],"complete":true,"errors":[]}` {
		t.Fatalf("%s", raw)
	}
}
