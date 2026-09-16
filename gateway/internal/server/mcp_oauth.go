package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"piso/gateway/internal/store"
)

const mcpOAuthCallbackPath = "/callback"

func (s *Server) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

func (s *Server) mcpRedirectURI() string {
	port := ingressHostPort()
	if port == 80 {
		return "http://" + mcpOAuthLabel + ".piso.local" + mcpOAuthCallbackPath
	}
	return fmt.Sprintf("http://%s.piso.local:%d%s", mcpOAuthLabel, port, mcpOAuthCallbackPath)
}

func (s *Server) startMCPOAuth(ctx context.Context, rec store.McpServerRec) (string, error) {
	if rec.AuthURL == "" || rec.TokenURL == "" {
		if err := s.discoverMCPOAuth(ctx, &rec); err != nil {
			return "", err
		}
	}
	if rec.ClientID == "" {
		if err := s.registerMCPClient(ctx, &rec); err != nil {
			return "", err
		}
	}
	verifier, err := randomURLToken(32)
	if err != nil {
		return "", err
	}
	state, err := randomURLToken(24)
	if err != nil {
		return "", err
	}
	rec.CodeVerifier = verifier
	rec.OAuthState = state
	if err := s.Store.ReplaceMcpServer(rec); err != nil {
		return "", err
	}
	u, err := url.Parse(rec.AuthURL)
	if err != nil {
		return "", fmt.Errorf("authorization endpoint: %w", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", rec.ClientID)
	q.Set("redirect_uri", s.mcpRedirectURI())
	q.Set("code_challenge", pkceS256(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	if rec.Resource != "" {
		q.Set("resource", rec.Resource)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *Server) handleMcpOAuthHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != mcpOAuthCallbackPath && r.URL.Path != mcpOAuthCallbackPath+"/" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if errStr := q.Get("error"); errStr != "" {
		http.Error(w, "MCP authorization error: "+errStr+" "+q.Get("error_description"), http.StatusBadRequest)
		return
	}
	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}
	rec, ok := s.Store.McpByState(state)
	if !ok {
		http.Error(w, "unknown OAuth state", http.StatusBadRequest)
		return
	}
	if err := s.exchangeMCPCode(r.Context(), &rec, code); err != nil {
		log.Printf("mcp oauth token exchange: %v", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>piso MCP</title>
<p>Authorization complete. You can close this tab and return to the dashboard.</p>`)
}

func (s *Server) exchangeMCPCode(ctx context.Context, rec *store.McpServerRec, code string) error {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", s.mcpRedirectURI())
	form.Set("code_verifier", rec.CodeVerifier)
	form.Set("client_id", rec.ClientID)
	if rec.Resource != "" {
		form.Set("resource", rec.Resource)
	}
	tok, err := s.postMCPToken(ctx, *rec, form)
	if err != nil {
		return err
	}
	return s.applyMCPTokens(ctx, rec, tok)
}

func (s *Server) refreshMCP(ctx context.Context, rec *store.McpServerRec) error {
	if rec.RefreshToken == "" || rec.TokenURL == "" {
		return fmt.Errorf("no refresh token")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rec.RefreshToken)
	form.Set("client_id", rec.ClientID)
	if rec.Resource != "" {
		form.Set("resource", rec.Resource)
	}
	tok, err := s.postMCPToken(ctx, *rec, form)
	if err != nil {
		return err
	}
	return s.applyMCPTokens(ctx, rec, tok)
}

type mcpTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func (s *Server) postMCPToken(ctx context.Context, rec store.McpServerRec, form url.Values) (mcpTokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rec.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return mcpTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if rec.ClientSecret != "" {
		req.SetBasicAuth(rec.ClientID, rec.ClientSecret)
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return mcpTokenResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mcpTokenResponse{}, fmt.Errorf("token endpoint HTTP %d", resp.StatusCode)
	}
	var tok mcpTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return mcpTokenResponse{}, fmt.Errorf("token json: %w", err)
	}
	if tok.AccessToken == "" {
		return mcpTokenResponse{}, fmt.Errorf("token response missing access_token")
	}
	return tok, nil
}

func (s *Server) applyMCPTokens(ctx context.Context, rec *store.McpServerRec, tok mcpTokenResponse) error {
	sec, ok := s.Store.SecretByID(rec.SecretID)
	if !ok {
		return fmt.Errorf("mcp secret missing")
	}
	sec.Value = tok.AccessToken
	if err := s.Store.ReplaceSecret(sec); err != nil {
		return err
	}
	if tok.RefreshToken != "" {
		rec.RefreshToken = tok.RefreshToken
	}
	rec.CodeVerifier = ""
	rec.OAuthState = ""
	rec.Status = store.McpStatusOK
	if tok.ExpiresIn > 0 {
		exp := time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second)
		rec.ExpiresAt = &exp
	} else {
		rec.ExpiresAt = nil
	}
	if err := s.Store.ReplaceMcpServer(*rec); err != nil {
		return err
	}
	if s.Proxy != nil {
		s.replayMatches(ctx, map[string]bool{}, rec.Placeholder, rec.Host)
	}
	return nil
}

type prMeta struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

type asMeta struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

func (s *Server) discoverMCPOAuth(ctx context.Context, rec *store.McpServerRec) error {
	u, err := url.Parse(rec.URL)
	if err != nil {
		return err
	}
	origin := u.Scheme + "://" + u.Host
	candidates := []string{
		origin + "/.well-known/oauth-protected-resource" + u.Path,
		origin + "/.well-known/oauth-protected-resource",
	}
	var pr prMeta
	var last error
	for _, cand := range candidates {
		if err := s.getJSON(ctx, cand, &pr); err != nil {
			last = err
			continue
		}
		last = nil
		break
	}
	if last != nil && pr.Resource == "" && len(pr.AuthorizationServers) == 0 {
		return fmt.Errorf("oauth protected resource metadata: %w", last)
	}
	as := origin
	if len(pr.AuthorizationServers) > 0 {
		as = strings.TrimRight(pr.AuthorizationServers[0], "/")
	}
	if rec.Resource == "" {
		if pr.Resource != "" {
			rec.Resource = pr.Resource
		} else {
			rec.Resource = rec.URL
		}
	}
	var meta asMeta
	if err := s.getJSON(ctx, as+"/.well-known/oauth-authorization-server", &meta); err != nil {
		return fmt.Errorf("authorization server metadata: %w", err)
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" {
		return fmt.Errorf("authorization server metadata missing endpoints")
	}
	rec.AuthURL = meta.AuthorizationEndpoint
	rec.TokenURL = meta.TokenEndpoint
	rec.RegisterURL = meta.RegistrationEndpoint
	return s.Store.ReplaceMcpServer(*rec)
}

type dcrResponse struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func (s *Server) registerMCPClient(ctx context.Context, rec *store.McpServerRec) error {
	if rec.RegisterURL == "" {
		return fmt.Errorf("authorization server has no registration endpoint")
	}
	body := map[string]any{
		"client_name":                "piso",
		"redirect_uris":              []string{s.mcpRedirectURI()},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "client_secret_basic",
		"code_challenge_methods":     []string{"S256"},
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rec.RegisterURL, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("DCR HTTP %d", resp.StatusCode)
	}
	var out dcrResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("DCR json: %w", err)
	}
	if out.ClientID == "" {
		return fmt.Errorf("DCR missing client_id")
	}
	rec.ClientID = out.ClientID
	rec.ClientSecret = out.ClientSecret
	return s.Store.ReplaceMcpServer(*rec)
}

func (s *Server) getJSON(ctx context.Context, rawURL string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s HTTP %d", rawURL, resp.StatusCode)
	}
	return json.Unmarshal(b, dest)
}

func pkceS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomURLToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
