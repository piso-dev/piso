package githubapp

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"piso/gateway/internal/store"
)

type cacheTransport func(*http.Request) (*http.Response, error)

func (f cacheTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMintCacheIsolationWithSharedInstallationID(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a := store.GithubAppRec{AppID: 1, PrivateKey: string(testPEM(t)), Installations: []store.GithubInstallation{{ID: 7, Account: "alpha"}}}
	b := store.GithubAppRec{AppID: 2, PrivateKey: string(testPEM(t)), Installations: []store.GithubInstallation{{ID: 7, Account: "beta"}}}
	expected := map[string]string{}
	for _, app := range []store.GithubAppRec{a, b} {
		jwt, err := SignJWT(app.AppID, []byte(app.PrivateKey), now)
		if err != nil {
			t.Fatal(err)
		}
		expected["Bearer "+jwt] = "token-" + app.Installations[0].Account
	}
	calls := 0
	c := NewClient(&http.Client{Transport: cacheTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		token, ok := expected[r.Header.Get("Authorization")]
		if !ok {
			t.Fatal("unexpected signing credentials")
		}
		raw, _ := json.Marshal(map[string]string{"token": token, "expires_at": "2099-01-01T00:00:00Z"})
		return &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})})
	c.Now = func() time.Time { return now }
	for _, app := range []store.GithubAppRec{a, a, b, b, a, b} {
		token, err := c.Mint(app, app.Installations[0].Account)
		if err != nil {
			t.Fatal(err)
		}
		if token != "token-"+app.Installations[0].Account {
			t.Fatal("cross-app token reuse")
		}
	}
	if calls != 4 {
		t.Fatalf("cache should miss on credential changes and hit on repeats: %d calls", calls)
	}
}
