package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"piso/gateway/internal/store"
)

type githubOAuthAttempt struct {
	verifier, browser, clientID string
	expires                     time.Time
}
type githubIdentity struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

func githubUserCallback() string { return githubAppPublicBase() + "/api/v1/github-user/callback" }

func (s *Server) githubUserJSON(r *http.Request, path, token string, out any) error {
	req, err := http.NewRequestWithContext(r.Context(), "GET", "https://api.github.com"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("GitHub is unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub returned HTTP %d; reconnect or check organization policy", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}
func (s *Server) githubUserToken() (string, bool) {
	u := s.Store.GithubUser()
	sec, ok := s.Store.SecretByID(u.TokenSecretID)
	return sec.Value, ok && sec.GatewayOnly && sec.Value != ""
}
func (s *Server) verifiedGithubUser(r *http.Request) (githubIdentity, error) {
	tok, ok := s.githubUserToken()
	var identity githubIdentity
	if !ok {
		return identity, fmt.Errorf("sign in to GitHub first")
	}
	if err := s.githubUserJSON(r, "/user", tok, &identity); err != nil {
		return identity, err
	}
	if identity.ID <= 0 || identity.ID != s.Store.GithubUser().AccountID || identity.Login == "" {
		return identity, fmt.Errorf("GitHub identity changed; sign in again")
	}
	return identity, nil
}

func (s *Server) saveGithubUserSecret(name, value string) (string, error) {
	id := "sec_" + randID()
	err := s.Store.AddSecret(store.SecretRec{ID: id, Name: name, Kind: "github-oauth", Placeholder: "piso_github_oauth_" + randID(), Value: value, GatewayOnly: true, CreatedAt: time.Now().UTC()})
	return id, err
}
func (s *Server) registerGithubUser(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/github-user", func(w http.ResponseWriter, r *http.Request) {
		u := s.Store.GithubUser()
		_, identityErr := s.verifiedGithubUser(r)
		connected := identityErr == nil
		secret, configured := s.Store.SecretByID(u.ClientSecretID)
		configured = configured && secret.GatewayOnly && secret.Value != ""
		writeJSON(w, 200, map[string]any{"configured": configured && u.ClientID != "", "connected": connected, "login": u.Login, "accountId": u.AccountID, "callbackUrl": githubUserCallback()})
	})
	mux.HandleFunc("PUT /api/v1/github-user/config", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ClientID     string `json:"clientId"`
			ClientSecret string `json:"clientSecret"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&in) != nil || strings.TrimSpace(in.ClientID) == "" || strings.TrimSpace(in.ClientSecret) == "" {
			writeJSON(w, 400, map[string]string{"error": "clientId and clientSecret required"})
			return
		}
		id, err := s.saveGithubUserSecret("GitHub OAuth client secret", strings.TrimSpace(in.ClientSecret))
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not save client secret"})
			return
		}
		old := s.Store.GithubUser()
		u := store.GithubUserRec{ClientID: strings.TrimSpace(in.ClientID), ClientSecretID: id}
		if s.Store.SetGithubUser(u) != nil {
			writeJSON(w, 500, map[string]string{"error": "could not save configuration"})
			return
		}
		if old.ClientSecretID != "" {
			s.Store.DeleteSecret(old.ClientSecretID)
		}
		if old.TokenSecretID != "" {
			s.Store.DeleteSecret(old.TokenSecretID)
		}
		w.WriteHeader(204)
	})
	// Top-level navigation sets the browser-binding cookie on the loopback callback origin.
	mux.HandleFunc("GET /api/v1/github-user/authorize", s.handleGithubUserAuthorize)
	mux.HandleFunc("GET /api/v1/github-user/callback", s.handleGithubUserCallback)
	mux.HandleFunc("DELETE /api/v1/github-user", func(w http.ResponseWriter, r *http.Request) {
		u := s.Store.GithubUser()
		id := u.TokenSecretID
		u.TokenSecretID = ""
		u.Login = ""
		u.AccountID = 0
		if s.Store.SetGithubUser(u) != nil {
			writeJSON(w, 500, map[string]string{"error": "could not disconnect"})
			return
		}
		if id != "" {
			s.Store.DeleteSecret(id)
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /api/v1/github-user/organizations", func(w http.ResponseWriter, r *http.Request) {
		identity, err := s.verifiedGithubUser(r)
		if err != nil {
			writeJSON(w, 401, map[string]string{"error": err.Error()})
			return
		}
		orgs, err := s.githubOwnedOrgs(r)
		if err != nil {
			writeJSON(w, 401, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, githubUserContext{Login: identity.Login, AccountID: identity.ID, Orgs: orgs})
	})
}
func (s *Server) handleGithubUserAuthorize(w http.ResponseWriter, r *http.Request) {
	u := s.Store.GithubUser()
	sec, ok := s.Store.SecretByID(u.ClientSecretID)
	if !ok || !sec.GatewayOnly || u.ClientID == "" {
		http.Error(w, "Configure a GitHub OAuth App first", 400)
		return
	}
	state, err := randomURLToken(32)
	if err != nil {
		http.Error(w, "random state unavailable", 500)
		return
	}
	verifier, err := randomURLToken(32)
	if err != nil {
		http.Error(w, "PKCE unavailable", 500)
		return
	}
	browser, err := randomURLToken(32)
	if err != nil {
		http.Error(w, "browser binding unavailable", 500)
		return
	}
	s.githubOAuthMu.Lock()
	if s.githubOAuthStates == nil {
		s.githubOAuthStates = map[string]githubOAuthAttempt{}
	}
	for key, a := range s.githubOAuthStates {
		if time.Now().After(a.expires) {
			delete(s.githubOAuthStates, key)
		}
	}
	s.githubOAuthStates[state] = githubOAuthAttempt{verifier: verifier, browser: browser, clientID: u.ClientID, expires: time.Now().Add(10 * time.Minute)}
	s.githubOAuthMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "piso_github_oauth", Value: browser, Path: "/api/v1/github-user", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {u.ClientID}, "redirect_uri": {githubUserCallback()}, "scope": {"read:user read:org"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+q.Encode(), 302)
}
func (s *Server) handleGithubUserCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	cookie, _ := r.Cookie("piso_github_oauth")
	s.githubOAuthMu.Lock()
	a, ok := s.githubOAuthStates[state]
	if ok && cookie != nil && cookie.Value == a.browser {
		delete(s.githubOAuthStates, state)
	} else {
		ok = false
	}
	s.githubOAuthMu.Unlock()
	if !ok || time.Now().After(a.expires) {
		http.Error(w, "Invalid or expired GitHub sign-in; start again", 400)
		return
	}
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		http.Error(w, "GitHub sign-in cancelled; return to the dashboard to retry", 400)
		return
	}
	u := s.Store.GithubUser()
	secret, exists := s.Store.SecretByID(u.ClientSecretID)
	if !exists || !secret.GatewayOnly || a.clientID != u.ClientID {
		http.Error(w, "OAuth configuration changed; start again", 400)
		return
	}
	values := url.Values{"client_id": {u.ClientID}, "client_secret": {secret.Value}, "code": {r.URL.Query().Get("code")}, "redirect_uri": {githubUserCallback()}, "code_verifier": {a.verifier}}
	req, _ := http.NewRequestWithContext(r.Context(), "POST", "https://github.com/login/oauth/access_token", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		http.Error(w, "GitHub token exchange failed", 502)
		return
	}
	defer resp.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token) != nil || token.AccessToken == "" || token.Error != "" {
		http.Error(w, "GitHub token exchange failed", 502)
		return
	}
	scopes := "," + strings.ReplaceAll(token.Scope, " ", ",") + ","
	if !strings.Contains(scopes, ",read:org,") {
		http.Error(w, "Organization discovery requires read:org; authorize again", 400)
		return
	}
	var identity githubIdentity
	if s.githubUserJSON(r, "/user", token.AccessToken, &identity) != nil || identity.ID <= 0 || identity.Login == "" {
		http.Error(w, "Could not verify GitHub identity", 502)
		return
	}
	id, err := s.saveGithubUserSecret("GitHub user OAuth — gateway only", token.AccessToken)
	if err != nil {
		http.Error(w, "Could not save GitHub connection", 500)
		return
	}
	old := u.TokenSecretID
	u.TokenSecretID = id
	u.Login = identity.Login
	u.AccountID = identity.ID
	if s.Store.SetGithubUser(u) != nil {
		http.Error(w, "Could not save GitHub connection", 500)
		return
	}
	if old != "" {
		s.Store.DeleteSecret(old)
	}
	http.SetCookie(w, &http.Cookie{Name: "piso_github_oauth", Value: "", Path: "/api/v1/github-user", HttpOnly: true, MaxAge: -1})
	http.Redirect(w, r, githubAppHomepage()+"/#github", 302)
}

// githubUserContext is what the signed-in person can act on: their own
// identity plus organizations where their membership role is admin and
// active (GitHub-side enforcement; a private app can only be created by an
// owner).
type githubUserContext struct {
	Login     string           `json:"login"`
	AccountID int64            `json:"accountId"`
	Orgs      []githubIdentity `json:"orgs"`
}

func (s *Server) githubOwnedOrgs(r *http.Request) ([]githubIdentity, error) {
	tok, ok := s.githubUserToken()
	if !ok {
		return nil, fmt.Errorf("sign in to GitHub first")
	}
	out := []githubIdentity{}
	for page := 1; ; page++ {
		var rows []struct {
			State        string         `json:"state"`
			Role         string         `json:"role"`
			Organization githubIdentity `json:"organization"`
		}
		if err := s.githubUserJSON(r, fmt.Sprintf("/user/memberships/orgs?state=active&per_page=100&page=%d", page), tok, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.State == "active" && row.Role == "admin" {
				org := row.Organization
				org.Type = "Organization"
				out = append(out, org)
			}
		}
		if len(rows) < 100 {
			return out, nil
		}
	}
}
