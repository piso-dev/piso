package githubapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"piso/gateway/internal/store"
)

const defaultAPI = "https://api.github.com"

// Client talks to api.github.com as a GitHub App.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Now     func() time.Time

	mu    sync.Mutex
	cache map[int64]cachedToken
}

type cachedToken struct {
	Token   string
	Expires time.Time
}

// NewClient returns a Client. httpClient nil uses http.DefaultClient.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		HTTP:    httpClient,
		BaseURL: defaultAPI,
		Now:     time.Now,
		cache:   map[int64]cachedToken{},
	}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) api() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultAPI
}

// Mint returns an installation access token for owner. Empty owner is
// allowed when the app has exactly one installation.
func (c *Client) Mint(app store.GithubAppRec, owner string) (string, error) {
	inst, err := pickInstallation(app, owner)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	if tok, ok := c.cache[inst.ID]; ok && tok.Token != "" && tok.Expires.After(c.now().Add(5*time.Minute)) {
		c.mu.Unlock()
		return tok.Token, nil
	}
	c.mu.Unlock()

	jwt, err := SignJWT(app.AppID, []byte(app.PrivateKey), c.now())
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.doJSON(http.MethodPost, "/app/installations/"+fmt.Sprintf("%d", inst.ID)+"/access_tokens", jwt, nil, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("github app: empty installation token")
	}
	if out.ExpiresAt.IsZero() {
		out.ExpiresAt = c.now().Add(50 * time.Minute)
	}
	c.mu.Lock()
	c.cache[inst.ID] = cachedToken{Token: out.Token, Expires: out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, nil
}

// LiveTokens returns cached installation tokens (for the real-secret scanner).
func (c *Client) LiveTokens() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	var out []string
	for id, tok := range c.cache {
		if tok.Token == "" || !tok.Expires.After(now) {
			delete(c.cache, id)
			continue
		}
		out = append(out, tok.Token)
	}
	return out
}

// FetchInstallation loads one installation (account login + html_url).
func (c *Client) FetchInstallation(app store.GithubAppRec, installationID int64) (store.GithubInstallation, error) {
	jwt, err := SignJWT(app.AppID, []byte(app.PrivateKey), c.now())
	if err != nil {
		return store.GithubInstallation{}, err
	}
	var raw struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}
	if err := c.doJSON(http.MethodGet, fmt.Sprintf("/app/installations/%d", installationID), jwt, nil, &raw); err != nil {
		return store.GithubInstallation{}, err
	}
	return store.GithubInstallation{
		ID: raw.ID, Account: strings.ToLower(raw.Account.Login),
		AccountType: raw.Account.Type, HTMLURL: raw.HTMLURL,
	}, nil
}

// ConvertManifest exchanges a manifest callback code for app credentials.
func (c *Client) ConvertManifest(code string) (appID int64, slug, pem, clientID, clientSecret, webhookSecret string, err error) {
	var raw struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		PEM           string `json:"pem"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if err := c.doJSON(http.MethodPost, "/app-manifests/"+code+"/conversions", "", nil, &raw); err != nil {
		return 0, "", "", "", "", "", err
	}
	if raw.ID == 0 || raw.PEM == "" {
		return 0, "", "", "", "", "", fmt.Errorf("github app: incomplete manifest conversion")
	}
	return raw.ID, raw.Slug, raw.PEM, raw.ClientID, raw.ClientSecret, raw.WebhookSecret, nil
}

// ListRepos returns full_names the installation can access (first page).
func (c *Client) ListRepos(app store.GithubAppRec, owner string) ([]string, error) {
	tok, err := c.Mint(app, owner)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := c.doJSON(http.MethodGet, "/installation/repositories?per_page=100", tok, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(raw.Repositories))
	for _, r := range raw.Repositories {
		if r.FullName != "" {
			out = append(out, r.FullName)
		}
	}
	return out, nil
}

func (c *Client) doJSON(method, path, bearer string, body []byte, dest any) error {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	} else if method == http.MethodPost {
		rdr = bytes.NewReader([]byte("{}"))
	}
	req, err := http.NewRequest(method, c.api()+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "piso-gateway")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("github api %s %s: %s %s", method, path, resp.Status, msg)
	}
	if dest == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dest)
}

func pickInstallation(app store.GithubAppRec, owner string) (store.GithubInstallation, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if owner == "" {
		if len(app.Installations) == 1 {
			return app.Installations[0], nil
		}
		if len(app.Installations) == 0 {
			return store.GithubInstallation{}, fmt.Errorf("github app: not installed")
		}
		return store.GithubInstallation{}, fmt.Errorf("github app: owner required")
	}
	for _, inst := range app.Installations {
		if strings.EqualFold(inst.Account, owner) {
			return inst, nil
		}
	}
	return store.GithubInstallation{}, fmt.Errorf("github app: no installation for %s", owner)
}

// PlaceholderMinter resolves a worker placeholder to an installation token.
type PlaceholderMinter struct {
	Store  *store.Store
	Client *Client
}

// NewPlaceholderMinter wires store + API client.
func NewPlaceholderMinter(st *store.Store, c *Client) *PlaceholderMinter {
	if c == nil {
		c = NewClient(nil)
	}
	return &PlaceholderMinter{Store: st, Client: c}
}

// Mint implements proxy.TokenMinter.
func (m *PlaceholderMinter) Mint(placeholder, host, path, body string) (string, error) {
	if m == nil || m.Store == nil || m.Client == nil {
		return "", fmt.Errorf("github app: minter not configured")
	}
	app, ok := m.Store.GithubAppByPlaceholder(placeholder)
	if !ok {
		return "", fmt.Errorf("github app: no app for placeholder")
	}
	return m.Client.Mint(app, Owner(host, path, body))
}

// LiveTokens implements proxy.TokenMinter.
func (m *PlaceholderMinter) LiveTokens() []string {
	if m == nil || m.Client == nil {
		return nil
	}
	return m.Client.LiveTokens()
}
