package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"piso/gateway/internal/model"
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

var errGithubUserSignIn = errors.New("sign in to GitHub first")

// githubOAuthSecretKind marks the gateway-only rows this file owns (the OAuth
// client secret and the user token). They are looked up and replaced by name
// so a re-auth or a config re-save never leaves an orphan behind. The value is
// shared with technocore, which materializes the same kind on a peer, so both
// sides must use one constant.
const githubOAuthSecretKind = model.SecretKindGithubOAuth

// githubScopeGranted reports whether GitHub granted scope want. GitHub returns
// the granted scopes as a space-separated list on the token exchange (and
// comma-separated in some API responses), so accept either.
func githubScopeGranted(granted, want string) bool {
	for _, s := range strings.FieldsFunc(granted, func(r rune) bool { return r == ' ' || r == ',' }) {
		if s == want {
			return true
		}
	}
	return false
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
	if resp.StatusCode == http.StatusUnauthorized {
		return errGithubUserSignIn
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub returned HTTP %d; retry or check organization policy", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// githubUserBundleRow finds the transport row for the shared sign-in. The row
// carries no value of its own: technocore encodes the bundle from the
// GithubUserRec and its gateway-only rows at share time, the same way it
// encodes a GitHub App group from its app records.
//
// A gateway can briefly hold more than one such row after a rebase; the one the
// session points at is the one to republish into, so the others are not
// resurrected as separate resources.
func (s *Server) githubUserBundleRow() (store.SecretRec, bool) {
	u := s.Store.GithubUser()
	var fallback store.SecretRec
	found := false
	for _, rec := range s.Store.Secrets() {
		if rec.Kind != model.SecretKindGithubUser {
			continue
		}
		if u.BundleID != "" && rec.ID == u.BundleID {
			return rec, true
		}
		if !found {
			fallback, found = rec, true
		}
	}
	return fallback, found
}

// ensureGithubUserBundle creates the shared-sign-in row if this gateway has
// none. It is created on sign-in, not at startup, so a gateway that never signs
// in never publishes an empty session.
//
// An existing row is reused rather than a second one minted: it already has a
// coordinator resource id, so the republish updates that session in place
// instead of publishing a rival copy.
func (s *Server) ensureGithubUserBundle() error {
	rec, ok := s.githubUserBundleRow()
	if !ok {
		rec = store.SecretRec{
			ID:          "sec_" + randID(),
			Name:        "GitHub sign-in",
			Kind:        model.SecretKindGithubUser,
			Placeholder: "piso_github_user_" + randID(),
			GatewayOnly: true,
			CreatedAt:   time.Now().UTC(),
		}
		if err := s.Store.AddSecret(rec); err != nil {
			return err
		}
	}
	u := s.Store.GithubUser()
	u.BundleID = rec.ID
	u.Source = store.GithubUserSourceLocal
	return s.Store.SetGithubUser(u)
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
		return identity, errGithubUserSignIn
	}
	if err := s.githubUserJSON(r, "/user", tok, &identity); err != nil {
		return identity, err
	}
	if identity.ID <= 0 || identity.Login == "" {
		return identity, fmt.Errorf("GitHub returned an incomplete identity; retry")
	}
	if identity.ID != s.Store.GithubUser().AccountID {
		return identity, fmt.Errorf("GitHub identity changed: %w", errGithubUserSignIn)
	}
	return identity, nil
}

// saveGithubUserSecret stores one gateway-only OAuth credential. It replaces
// the existing secret with the same name and kind on this gateway instead of
// inserting a new row: a re-auth or a config re-save would otherwise leave an
// orphaned "GitHub user OAuth — gateway only" secret behind, and the id the
// GithubUser record points at has to keep resolving. Any other duplicate with
// the same name is removed.
func (s *Server) saveGithubUserSecret(name, value string) (string, error) {
	keep := ""
	for _, rec := range s.Store.Secrets() {
		if rec.Kind != githubOAuthSecretKind || rec.Name != name {
			continue
		}
		if keep == "" {
			rec.Value = value
			rec.GatewayOnly = true
			if !strings.HasPrefix(rec.Placeholder, "piso_") {
				rec.Placeholder = "piso_github_oauth_" + randID()
			}
			if err := s.Store.ReplaceSecret(rec); err != nil {
				return "", err
			}
			keep = rec.ID
			continue
		}
		// A duplicate from an older insert: the one we kept now owns the name.
		_ = s.Store.DeleteSecret(rec.ID)
	}
	if keep != "" {
		return keep, nil
	}
	id := "sec_" + randID()
	err := s.storeSecret(store.SecretRec{ID: id, Name: name, Kind: githubOAuthSecretKind, Placeholder: "piso_github_oauth_" + randID(), Value: value, GatewayOnly: true, CreatedAt: time.Now().UTC()})
	return id, err
}
func (s *Server) registerGithubUser(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/github-user", func(w http.ResponseWriter, r *http.Request) {
		u := s.Store.GithubUser()
		_, identityErr := s.verifiedGithubUser(r)
		// An unavailable verification is not evidence of a revoked token.
		// Return an error so the dashboard retains its last known session state.
		if identityErr != nil && !errors.Is(identityErr, errGithubUserSignIn) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub sign-in verification is unavailable; retry shortly"})
			return
		}
		connected := identityErr == nil
		secret, configured := s.Store.SecretByID(u.ClientSecretID)
		configured = configured && secret.GatewayOnly && secret.Value != ""
		// orgAccess is a tri-state: true/false once the granted scopes are
		// known, null for an older connection that never recorded them. The
		// dashboard must not warn about a missing scope it cannot see.
		var orgAccess any
		if strings.TrimSpace(u.Scopes) != "" {
			orgAccess = githubScopeGranted(u.Scopes, "read:org")
		}
		// source tells the dashboard whether this session was signed in here or
		// arrived from another enrolled gateway. Either way it is one account
		// session, and signing out is global.
		//
		// clientId is the OAuth App's public identifier (it is already visible in
		// the authorize URL), returned so the configuration dialog can show which
		// app is in use instead of opening blank. The client secret stays in the
		// vault and is never returned.
		writeJSON(w, 200, map[string]any{"configured": configured && u.ClientID != "", "connected": connected, "login": u.Login, "accountId": u.AccountID, "clientId": u.ClientID, "orgAccess": orgAccess, "source": u.SourceOrLocal(), "callbackUrl": githubUserCallback()})
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
		clientID := strings.TrimSpace(in.ClientID)
		old := s.Store.GithubUser()
		id, err := s.saveGithubUserSecret("GitHub OAuth client secret", strings.TrimSpace(in.ClientSecret))
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not save client secret"})
			return
		}
		// Replacing the client secret must not sign the user out: the token
		// minted by the same OAuth App is still valid. Only a different client
		// ID invalidates it, because the token belongs to the old app.
		u := old
		u.ClientID = clientID
		u.ClientSecretID = id
		changedApp := old.ClientID != "" && old.ClientID != clientID
		if changedApp {
			u.TokenSecretID = ""
			u.Login = ""
			u.AccountID = 0
		}
		if s.Store.SetGithubUser(u) != nil {
			writeJSON(w, 500, map[string]string{"error": "could not save configuration"})
			return
		}
		if old.ClientSecretID != "" && old.ClientSecretID != id {
			s.Store.DeleteSecret(old.ClientSecretID)
		}
		if changedApp && old.TokenSecretID != "" {
			s.Store.DeleteSecret(old.TokenSecretID)
		}
		s.publishShared()
		w.WriteHeader(204)
	})
	// Top-level navigation sets the browser-binding cookie on the loopback callback origin.
	mux.HandleFunc("GET /api/v1/github-user/authorize", s.handleGithubUserAuthorize)
	mux.HandleFunc("GET /api/v1/github-user/callback", s.handleGithubUserCallback)
	// Signing out is global: the session is one account session, shared with
	// every enrolled gateway, so the tombstone is published and each peer drops
	// its copy. The OAuth App configuration stays, so signing back in needs no
	// re-setup.
	mux.HandleFunc("DELETE /api/v1/github-user", func(w http.ResponseWriter, r *http.Request) {
		u := s.Store.GithubUser()
		id := u.TokenSecretID
		u.TokenSecretID = ""
		u.Login = ""
		u.AccountID = 0
		u.Scopes = ""
		u.Source = ""
		u.BundleID = ""
		if s.Store.SetGithubUser(u) != nil {
			writeJSON(w, 500, map[string]string{"error": "could not disconnect"})
			return
		}
		if rec, ok := s.githubUserBundleRow(); ok {
			// Revoking the transport row stops reconcile from republishing the
			// session and tells every peer to forget it.
			s.revokeOrDelete(rec)
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
	// A missing read:org is not a failed sign-in. The token still identifies
	// the person (read:user), and organization discovery is the only feature
	// that needs read:org. Record the grant and let the dashboard explain how
	// to widen it, instead of rejecting a valid login.
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
	u.Scopes = token.Scope
	if s.Store.SetGithubUser(u) != nil {
		http.Error(w, "Could not save GitHub connection", 500)
		return
	}
	if old != "" && old != id {
		s.Store.DeleteSecret(old)
	}
	// Share the session with the other enrolled gateways: sign in once here and
	// every gateway's dashboard is signed in with the same account.
	if err := s.ensureGithubUserBundle(); err == nil {
		s.publishShared()
	} else {
		log.Printf("github-user: share sign-in: %v", err)
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
	// If the recorded grant is known to lack read:org, say how to fix it
	// rather than surfacing a bare 403 from the memberships API. An empty
	// scope list means an older connection: fall through and let GitHub answer.
	u := s.Store.GithubUser()
	if strings.TrimSpace(u.Scopes) != "" && !githubScopeGranted(u.Scopes, "read:org") {
		return nil, fmt.Errorf("organization discovery needs the read:org scope; re-authorize and grant organization access, and if an organization blocks third-party apps an owner must approve this OAuth App under Organization settings → Third-party access")
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
