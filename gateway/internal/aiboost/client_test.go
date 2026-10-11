package aiboost

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const callback = "https://piso.example/oauth/ai-boost/callback"

func fixture(t *testing.T) (*Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	refreshes := &atomic.Int32{}
	registrations := &atomic.Int32{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/register/":
			registrations.Add(1)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"client_id": "piso", "client_secret": "client-secret-never-public", "redirect_uris": []string{callback}})
		case "/oauth/token/":
			id, secret, ok := r.BasicAuth()
			if !ok || id != "piso" || secret != "client-secret-never-public" {
				t.Error("missing Basic auth")
			}
			r.ParseForm()
			if r.Form.Get("grant_type") == "refresh_token" {
				refreshes.Add(1)
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "access-token-never-public", "refresh_token": "aib_refresh_never-public", "token_type": "Bearer", "scope": Scope, "expires_in": 900, "refresh_token_expires_in": 2592000})
		case "/api/library/boosters/":
			if r.Header.Get("Authorization") != "Bearer access-token-never-public" {
				t.Error("missing bearer")
			}
			io.WriteString(w, `{"items":[],"next":null,"previous":null}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := Open(t.TempDir(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Configure(context.Background(), srv.URL, callback); err != nil {
		t.Fatal(err)
	}
	return c, refreshes, registrations
}
func connect(t *testing.T, c *Client) {
	t.Helper()
	raw, err := c.Start("browser")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	if err = c.Callback(context.Background(), u.Query().Get("state"), "browser", "code", ""); err != nil {
		t.Fatal(err)
	}
}
func TestPKCECallbackAndEncryptedPersistence(t *testing.T) {
	c, _, registered := fixture(t)
	if err := c.Configure(context.Background(), c.rec.BaseURL, callback); err != nil || registered.Load() != 1 {
		t.Fatal("registration is not idempotent", err)
	}
	raw, err := c.Start("browser")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	a := c.attempts[q.Get("state")]
	sum := sha256.Sum256([]byte(a.verifier))
	if q.Get("scope") != Scope || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatal("invalid PKCE", q)
	}
	if err = c.Callback(context.Background(), q.Get("state"), "other-browser", "code", ""); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	if err = c.Callback(context.Background(), q.Get("state"), "browser", "code", ""); err != nil {
		t.Fatal(err)
	}
	if err = c.Callback(context.Background(), q.Get("state"), "browser", "code", ""); !errors.Is(err, ErrState) {
		t.Fatal("replay accepted", err)
	}
	disk, err := os.ReadFile(filepath.Join(c.vault.dir, "aiboost.enc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{c.rec.ClientSecret, c.rec.AccessToken, c.rec.RefreshToken} {
		if strings.Contains(string(disk), value) {
			t.Fatal("plaintext credential persisted")
		}
	}
	reopened, err := Open(c.vault.dir, c.http)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.rec.RefreshToken != c.rec.RefreshToken || reopened.Summary().Status != "ok" {
		t.Fatal("tokens did not survive restart")
	}
	summary, _ := json.Marshal(c.Summary())
	if strings.Contains(string(summary), "never-public") {
		t.Fatal("summary leaked tokens")
	}
	for _, name := range []string{"aiboost.key", "aiboost.enc"} {
		info, err := os.Stat(filepath.Join(c.vault.dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("vault permissions", err)
		}
	}
}
func TestExpiredAndDeniedState(t *testing.T) {
	c, _, _ := fixture(t)
	raw, _ := c.Start("browser")
	u, _ := url.Parse(raw)
	state := u.Query().Get("state")
	a := c.attempts[state]
	a.expires = time.Now().Add(-time.Second)
	c.attempts[state] = a
	if err := c.Callback(context.Background(), state, "browser", "code", ""); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	raw, _ = c.Start("browser")
	u, _ = url.Parse(raw)
	state = u.Query().Get("state")
	if err := c.Callback(context.Background(), state, "browser", "", "access_denied"); !errors.Is(err, ErrDeclined) {
		t.Fatal(err)
	}
	if c.Summary().Status == "ok" || len(c.attempts) != 0 {
		t.Fatal("denial created connection or retained state")
	}
}
func TestConcurrentRefreshAndCrashMarker(t *testing.T) {
	c, refreshes, _ := fixture(t)
	connect(t, c)
	c.rec.AccessExpires = time.Now().Add(-time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.List(context.Background(), "go", ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes=%d", refreshes.Load())
	}
	r := c.rec
	r.RefreshPending = true
	if err := c.commit(r); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(c.vault.dir, c.http)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Summary().Status != "needs-auth" || reopened.rec.RefreshToken != "" {
		t.Fatal("uncertain refresh reused after restart")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}
func TestInvalidGrantAndAmbiguousRefreshDisconnect(t *testing.T) {
	for _, network := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid-grant", true: "network"}[network], func(t *testing.T) {
			c, _, _ := fixture(t)
			connect(t, c)
			c.rec.AccessExpires = time.Now().Add(-time.Second)
			var calls int
			c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if network {
					return nil, errors.New("secret in transport error")
				}
				return response(400, `{"error":"invalid_grant","description":"secret"}`), nil
			})
			for i := 0; i < 2; i++ {
				if _, err := c.List(context.Background(), "", ""); !errors.Is(err, ErrReconnect) {
					t.Fatal(err)
				}
			}
			if calls != 1 || c.rec.RefreshToken != "" || c.Summary().Status != "needs-auth" {
				t.Fatal("refresh replayed or tokens retained")
			}
		})
	}
}
func Test401RetryAndDraftIdempotency(t *testing.T) {
	c, _, _ := fixture(t)
	connect(t, c)
	transport := c.http.Transport
	var attempts int
	var body string
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth/token/" {
			return transport.RoundTrip(r)
		}
		attempts++
		raw, _ := io.ReadAll(r.Body)
		if attempts == 1 {
			body = string(raw)
			return response(401, `{}`), nil
		}
		if r.Header.Get("Idempotency-Key") != "draft-uuid" || string(raw) != body {
			t.Error("retry changed idempotency or payload")
		}
		return response(201, `{"id":"draft","status":"submitted","revision":1}`), nil
	})
	out, err := c.Draft(context.Background(), DraftPayload{Name: "Go", Description: "Go guidance", Rationale: "Useful guidance", Content: Content{"markdown", "# Go"}}, "draft-uuid")
	if err != nil || out.ID != "draft" || attempts != 2 {
		t.Fatal(out, err, attempts)
	}
}
func TestDisconnectWaitsForRefreshThenErasesTokens(t *testing.T) {
	c, _, _ := fixture(t)
	connect(t, c)
	c.rec.AccessExpires = time.Now().Add(-time.Second)
	entered, release := make(chan struct{}), make(chan struct{})
	transport := c.http.Transport
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth/token/" {
			close(entered)
			<-release
		}
		return transport.RoundTrip(r)
	})
	done := make(chan struct{})
	go func() { defer close(done); _, _ = c.List(context.Background(), "", "") }()
	<-entered
	disconnected := make(chan error, 1)
	go func() { disconnected <- c.Disconnect() }()
	close(release)
	<-done
	if err := <-disconnected; err != nil {
		t.Fatal(err)
	}
	reloaded, err := Open(c.vault.dir, c.http)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.rec.AccessToken != "" || reloaded.rec.RefreshToken != "" || reloaded.Summary().Status != "none" {
		t.Fatal("in-flight refresh resurrected connection")
	}
}
func TestRedirectDoesNotLeakToken(t *testing.T) {
	c, _, _ := fixture(t)
	connect(t, c)
	var targetCalls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer origin.Close()
	c.rec.BaseURL = origin.URL
	if _, err := c.List(context.Background(), "", ""); err == nil {
		t.Fatal("redirect accepted")
	}
	if targetCalls.Load() != 0 {
		t.Fatal("credentials sent to redirect target")
	}
}
func TestPersistenceFailureNeverRetriesConsumedRefresh(t *testing.T) {
	c, _, _ := fixture(t)
	connect(t, c)
	c.rec.AccessExpires = time.Now().Add(-time.Second)
	transport := c.http.Transport
	var calls int
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := transport.RoundTrip(r)
		if r.URL.Path == "/oauth/token/" {
			calls++
			c.vault.dir = filepath.Join(c.vault.dir, "missing")
		}
		return resp, err
	})
	if _, err := c.List(context.Background(), "", ""); err == nil {
		t.Fatal("persistence failure ignored")
	}
	if _, err := c.List(context.Background(), "", ""); !errors.Is(err, ErrReconnect) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("consumed refresh retried")
	}
}
