package githubapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

type reposTransport func(*http.Request) (*http.Response, error)

func (f reposTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestListReposContextCancellation(t *testing.T) {
	app := store.GithubAppRec{AppID: 1, PrivateKey: string(testPEM(t)), Installations: []store.GithubInstallation{{ID: 7, Account: "demo"}}}
	for _, stage := range []string{"mint", "pages"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := NewClient(&http.Client{Transport: reposTransport(func(r *http.Request) (*http.Response, error) {
				if stage == "pages" && r.Method == http.MethodPost {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"token":"test-token","expires_at":"2099-01-01T00:00:00Z"}`))}, nil
				}
				cancel()
				if r.Context().Err() == nil {
					t.Error("request did not inherit caller cancellation")
					return nil, errors.New("missing request context")
				}
				return nil, r.Context().Err()
			})})
			if _, err := client.ListReposContext(ctx, app, "demo"); !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation during %s, got %v", stage, err)
			}
		})
	}
}
