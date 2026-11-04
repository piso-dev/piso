package gitaccess

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestGitPaginationPartialAndSafeErrors(t *testing.T) {
	for _, provider := range []string{"github", "gitea"} {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "Bearer secret" {
					t.Fatal("token missing")
				}
				if calls == 2 {
					return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("secret rejected")), Header: http.Header{}}, nil
				}
				repos := []string{}
				for i := 0; i < 100; i++ {
					repos = append(repos, fmt.Sprintf(`{"full_name":"org/repo%d"}`, i))
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[" + strings.Join(repos, ",") + "]")), Header: http.Header{}}, nil
			})}
			out := List(context.Background(), client, Config{Provider: provider, BaseURL: "https://git.test"}, "secret", "sec_one")
			if out.Complete || len(out.Repositories) != 100 || len(out.Errors) != 1 || strings.Contains(out.Errors[0].Message, "secret") {
				t.Fatalf("bad partial result: %+v", out)
			}
		})
	}
}
func TestGitDiscoveryRejectsRedirect(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://evil.test"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	out := List(context.Background(), client, Config{Provider: "github"}, "secret", "id")
	if calls != 1 || out.Complete || len(out.Errors) != 1 {
		t.Fatal("redirect followed or error hidden")
	}
}
func TestGitConfigRejectsUnsafeURLs(t *testing.T) {
	for _, base := range []string{"http://git.test", "https://user:pass@git.test", "https://*.test", "https://git.test?a=b", "https://git.test/#fragment", "https://git.test/../other"} {
		if _, _, err := (Config{Provider: "gitea", BaseURL: base}).Normalize(); err == nil {
			t.Fatalf("accepted %s", base)
		}
	}
}
