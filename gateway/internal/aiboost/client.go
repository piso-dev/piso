package aiboost

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const Scope = "boosters:read boosters:suggest"
const CallbackPath = "/oauth/ai-boost/callback"

var ErrReconnect = errors.New("AI Boost needs reconnecting in Connections")
var ErrState = errors.New("invalid or expired AI Boost sign-in; start again")
var ErrDeclined = errors.New("AI Boost connection declined")

type attempt struct {
	verifier, browser string
	expires           time.Time
}

// Client is one connection per gateway process. The mutex serializes refresh,
// callback, and disconnect, including persistence and authenticated requests.
// Only one gateway process may own a vault directory.
type Client struct {
	mu       sync.Mutex
	vault    *vault
	rec      record
	http     *http.Client
	attempts map[string]attempt
}
type Summary struct {
	Configured  bool   `json:"configured"`
	BaseURL     string `json:"baseUrl,omitempty"`
	RedirectURI string `json:"redirectUri,omitempty"`
	Status      string `json:"status"`
}

func Open(dir string, transport *http.Client) (*Client, error) {
	v, r, err := openVault(dir)
	if err != nil {
		return nil, err
	}
	h := &http.Client{}
	if transport != nil {
		*h = *transport
	}
	h.Timeout = 20 * time.Second
	// Never send codes, client secrets, or Bearer tokens to a redirect target.
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c := &Client{vault: v, rec: r, http: h, attempts: map[string]attempt{}}
	if r.RefreshPending {
		if err := c.disconnect("needs-auth"); err != nil {
			return nil, err
		}
	}
	return c, nil
}
func (c *Client) Summary() Summary {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := c.rec.Status
	if status == "ok" && !time.Now().Before(c.rec.RefreshExpires) {
		status = "needs-auth"
	}
	return Summary{c.rec.ClientID != "", c.rec.BaseURL, c.rec.RedirectURI, status}
}
func validateConfig(base, redirect string) error {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("AI Boost base URL must be an HTTPS origin")
	}
	r, err := url.Parse(redirect)
	if err != nil || r.Hostname() == "" || r.User != nil || r.Fragment != "" || r.RawQuery != "" || r.Path != CallbackPath {
		return errors.New("callback must be an absolute URL ending in " + CallbackPath)
	}
	loopback := r.Hostname() == "127.0.0.1" || r.Hostname() == "localhost" || r.Hostname() == "::1"
	// Permit only the dashboard apex, never worker subdomains or other .local hosts.
	localDashboard := r.Hostname() == "piso.local"
	if r.Scheme != "https" && !(r.Scheme == "http" && (loopback || localDashboard)) {
		return errors.New("callback requires HTTPS (HTTP is allowed only for loopback or piso.local)")
	}
	return nil
}

// Configure registers once. Configuration drift is rejected rather than
// silently orphaning an existing registration or replacing a live grant.
func (c *Client) Configure(ctx context.Context, base, redirect string) error {
	if err := validateConfig(base, redirect); err != nil {
		return err
	}
	base = strings.TrimRight(base, "/")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec.ClientID != "" {
		if c.rec.BaseURL == base && c.rec.RedirectURI == redirect {
			return nil
		}
		return errors.New("AI Boost is already registered with a different configuration")
	}
	body, _ := json.Marshal(map[string]any{"client_name": "Piso", "redirect_uris": []string{redirect}})
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/oauth/register/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("AI Boost registration unavailable")
	}
	defer resp.Body.Close()
	var out struct {
		ClientID     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if resp.StatusCode != 201 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil || out.ClientID == "" || out.ClientSecret == "" {
		return errors.New("AI Boost registration failed")
	}
	if len(out.RedirectURIs) != 1 || out.RedirectURIs[0] != redirect {
		return errors.New("AI Boost registered a different callback")
	}
	return c.commit(record{BaseURL: base, RedirectURI: redirect, ClientID: out.ClientID, ClientSecret: out.ClientSecret, Status: "none"})
}
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func (c *Client) Start(browser string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec.ClientID == "" || browser == "" {
		return "", errors.New("configure AI Boost first")
	}
	state, err := randomToken()
	if err != nil {
		return "", err
	}
	verifier, err := randomToken()
	if err != nil {
		return "", err
	}
	for k, a := range c.attempts {
		if time.Now().After(a.expires) {
			delete(c.attempts, k)
		}
	}
	if len(c.attempts) >= 32 {
		return "", errors.New("too many pending AI Boost sign-ins")
	}
	c.attempts[state] = attempt{verifier, browser, time.Now().Add(10 * time.Minute)}
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {c.rec.ClientID}, "redirect_uri": {c.rec.RedirectURI}, "response_type": {"code"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "scope": {Scope}, "state": {state}}
	return c.rec.BaseURL + "/oauth/authorize/?" + q.Encode(), nil
}
func (c *Client) Callback(ctx context.Context, state, browser, code, oauthError string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Validate error callbacks too. Unknown/mismatched callbacks cannot consume
	// another browser's pending transaction.
	var a attempt
	found := false
	for key, pending := range c.attempts {
		if subtle.ConstantTimeCompare([]byte(key), []byte(state)) == 1 && subtle.ConstantTimeCompare([]byte(pending.browser), []byte(browser)) == 1 {
			a = pending
			found = true
			delete(c.attempts, key)
			break
		}
	}
	if !found || !time.Now().Before(a.expires) {
		return ErrState
	}
	if oauthError != "" {
		return ErrDeclined
	}
	if code == "" {
		return ErrState
	}
	tok, err := c.token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.rec.RedirectURI}, "code_verifier": {a.verifier}})
	if err != nil {
		return err
	}
	return c.apply(tok)
}

type tokens struct {
	Access         string `json:"access_token"`
	Refresh        string `json:"refresh_token"`
	Expires        int64  `json:"expires_in"`
	RefreshExpires int64  `json:"refresh_token_expires_in"`
	Type           string `json:"token_type"`
	Scope          string `json:"scope"`
	obtained       time.Time
}
type tokenError struct{ code string }

func (e *tokenError) Error() string { return "AI Boost token exchange failed" }
func (c *Client) token(ctx context.Context, form url.Values) (tokens, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", c.rec.BaseURL+"/oauth/token/", strings.NewReader(form.Encode()))
	// RFC 6749 2.3.1 encodes each credential before constructing Basic auth.
	req.SetBasicAuth(url.QueryEscape(c.rec.ClientID), url.QueryEscape(c.rec.ClientSecret))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	obtained := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return tokens{}, errors.New("AI Boost token exchange unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var out struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
		// Only preserve recognized error codes, never upstream text.
		code := "failed"
		if out.Error == "invalid_grant" || out.Error == "invalid_client" {
			code = out.Error
		}
		return tokens{}, &tokenError{code}
	}
	var out tokens
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil || out.Access == "" || out.Refresh == "" || !strings.EqualFold(out.Type, "Bearer") || out.Expires <= 0 || out.Expires > 900 || out.RefreshExpires <= 0 || out.RefreshExpires > 90*86400 || !validScope(out.Scope) {
		return tokens{}, errors.New("invalid AI Boost token response")
	}
	out.obtained = obtained
	return out, nil
}
func validScope(s string) bool {
	fields := strings.Fields(s)
	return len(fields) == 2 && ((fields[0] == "boosters:read" && fields[1] == "boosters:suggest") || (fields[1] == "boosters:read" && fields[0] == "boosters:suggest"))
}
func (c *Client) commit(r record) error {
	if err := c.vault.save(r); err != nil {
		return errors.New("could not persist AI Boost connection")
	}
	c.rec = r
	return nil
}
func (c *Client) apply(t tokens) error {
	r := c.rec
	r.AccessToken = t.Access
	r.RefreshToken = t.Refresh
	r.AccessExpires = t.obtained.Add(time.Duration(t.Expires) * time.Second)
	r.RefreshExpires = t.obtained.Add(time.Duration(t.RefreshExpires) * time.Second)
	r.Status = "ok"
	r.RefreshPending = false
	return c.commit(r)
}
func (c *Client) disconnect(status string) error {
	c.attempts = map[string]attempt{}
	r := c.rec
	r.Status = status
	r.AccessToken = ""
	r.RefreshToken = ""
	r.AccessExpires = time.Time{}
	r.RefreshExpires = time.Time{}
	r.RefreshPending = false
	// Immediately fail closed even if storage is unavailable.
	c.rec = r
	return c.commit(r)
}
func (c *Client) Disconnect() error { c.mu.Lock(); defer c.mu.Unlock(); return c.disconnect("none") }
func (c *Client) refresh(ctx context.Context) error {
	if c.rec.RefreshPending || c.rec.Status != "ok" || !time.Now().Before(c.rec.RefreshExpires) {
		_ = c.disconnect("needs-auth")
		return ErrReconnect
	}
	r := c.rec
	r.RefreshPending = true
	if err := c.commit(r); err != nil {
		return err
	}
	tok, err := c.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {r.RefreshToken}})
	if err != nil {
		// Includes invalid_grant and ambiguous responses: never replay a refresh.
		_ = c.disconnect("needs-auth")
		return ErrReconnect
	}
	if err = c.apply(tok); err != nil {
		// Keep the durable refresh-pending marker; startup also fails closed.
		c.rec.Status = "needs-auth"
		c.rec.AccessToken = ""
		c.rec.RefreshToken = ""
		return err
	}
	return nil
}

// APIError deliberately excludes upstream bodies and headers.
type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("AI Boost API returned HTTP %d", e.Status) }

// request is deliberately private. Callers cannot choose origins, methods, or
// arbitrary endpoints (in particular, approval/publishing endpoints).
func (c *Client) request(ctx context.Context, method, path string, body []byte, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec.Status != "ok" {
		return nil, ErrReconnect
	}
	if !time.Now().Add(60 * time.Second).Before(c.rec.AccessExpires) {
		if err := c.refresh(ctx); err != nil {
			return nil, err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.rec.BaseURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, errors.New("invalid AI Boost request")
		}
		req.Header.Set("Authorization", "Bearer "+c.rec.AccessToken)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, errors.New("AI Boost API unavailable")
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
		resp.Body.Close()
		if resp.StatusCode == 401 {
			if attempt == 0 {
				if err := c.refresh(ctx); err != nil {
					return nil, err
				}
				continue
			}
			_ = c.disconnect("needs-auth")
			return nil, ErrReconnect
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &APIError{resp.StatusCode}
		}
		if readErr != nil || len(raw) > 2<<20 {
			return nil, errors.New("invalid AI Boost API response")
		}
		return raw, nil
	}
	return nil, ErrReconnect
}
