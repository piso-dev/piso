package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"piso/internal/authlink"
	"piso/internal/hostauth"
	tc "piso/internal/technocore"
)

// Dashboard authentication.
//
// The dashboard's control API used to be gated by the peer address alone: a
// request from anywhere but the worker VPC was answered like a loopback client.
// That is fine while the control plane is only reachable from the host, and it
// stops being fine the moment a front proxy publishes the dashboard on a public
// hostname — Caddy is deliberately a *trusted* peer, so every request that
// arrived through it looked local. This file adds the missing half: a real
// credential, checked per request, with the peer check kept as the outer ring.
//
// Three planes are kept structurally apart, and the ordering in WebHandler is
// what keeps them apart:
//
//	humans    the dashboard HTML and /api/v1/*      -> gated here
//	machines  the worker TLS mux (:8443, own CA)    -> a different handler
//	callbacks GitHub / MCP / AI-boost OAuth returns -> exempt, they carry their
//	                                                  own state + binding cookie
//
// A local gateway is unaffected: nothing is ever gated unless the request
// actually arrived from off-host (through the front proxy, or addressed to the
// configured public dashboard name).
const (
	envDashboardAuth = "PISO_DASHBOARD_AUTH"
	envDashboardHost = "PISO_DASHBOARD_HOST"

	sessionCookie  = "__Host-piso_sess"
	ceremonyCookie = "__Host-piso_ceremony"

	sessionTTL  = 12 * time.Hour
	ceremonyTTL = 5 * time.Minute
)

// ctxSessionKey carries the authenticated dashboard session to handlers.
type ctxSessionKey struct{}

// withSession attaches a verified session to the request context.
func withSession(ctx context.Context, sess dashSession) context.Context {
	return context.WithValue(ctx, ctxSessionKey{}, sess)
}

// ctxHostAuthKey marks a request the gate accepted on a host signature.
type ctxHostAuthKey struct{}

// withHostAuth records that the gate verified a host signature, so the handler
// does not consume the nonce a second time.
func withHostAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxHostAuthKey{}, true)
}

func hostAuthedFromContext(ctx context.Context) bool {
	ok, _ := ctx.Value(ctxHostAuthKey{}).(bool)
	return ok
}

// sessionFrom returns the session the gate verified for this request.
func sessionFrom(ctx context.Context) (dashSession, bool) {
	sess, ok := ctx.Value(ctxSessionKey{}).(dashSession)
	return sess, ok
}

// dashSession is the cookie payload. It is signed, not encrypted: everything in
// it is already visible to the holder.
type dashSession struct {
	Cred  string `json:"c,omitempty"`
	Kind  string `json:"k"`
	Epoch int    `json:"e"`
	Iat   int64  `json:"i"`
	Exp   int64  `json:"x"`
}

func (d dashSession) expired(now time.Time) bool { return now.Unix() >= d.Exp }

// authRequiredFor reports whether this request must authenticate, and why. The
// reason is user-visible (a 401 header and `piso gateway auth status`) so it
// says which rule fired.
//
// Precedence:
//  1. PISO_DASHBOARD_AUTH — the operator's explicit choice, both directions.
//  2. the public dashboard hostname — a request addressed to it came from off-host.
//  3. a peer inside the front-proxy subnet (PISO_TRUSTED_PEER_CIDRS) — the only
//     way Caddy's traffic can be told apart from the host's own CLI.
func (s *Server) authRequiredFor(r *http.Request) (bool, string) {
	if on, set := dashboardAuthOverride(); set {
		if on {
			return true, envDashboardAuth + "=on"
		}
		return false, envDashboardAuth + "=off"
	}
	if host := s.dashboardHost(); host != "" && hostOnly(r.Host) == host {
		return true, "request addressed to the public dashboard host " + host
	}
	if ip := parseRemoteIP(r.RemoteAddr); ip != nil && !ip.IsLoopback() && peerTrusted(ip) {
		return true, "request arrived through the TLS front proxy"
	}
	return false, ""
}

// dashboardAuthOverride reads PISO_DASHBOARD_AUTH. When the operator sets it,
// it wins in both directions — loopback included — so the gate and `piso
// gateway auth status` can never disagree.
func dashboardAuthOverride() (on, set bool) {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(envDashboardAuth))); v {
	case "1", "on", "true", "yes":
		return true, true
	case "0", "off", "false", "no":
		return false, true
	}
	return false, false
}

// dashboardPolicy reports whether this gateway needs a dashboard login at all,
// independent of who is asking. It exists so `piso gateway auth status` can
// answer the question from loopback, where the gate itself stays open.
func (s *Server) dashboardPolicy() (bool, string) {
	if on, set := dashboardAuthOverride(); set {
		if on {
			return true, envDashboardAuth + "=on"
		}
		return false, envDashboardAuth + "=off"
	}
	if host := s.dashboardHost(); host != "" {
		return true, "public dashboard host " + host
	}
	if nets := trustedPeerCIDRs(); len(nets) > 0 {
		return true, "requests from the front-proxy subnet are gated"
	}
	return false, ""
}

// dashboardHost is the public name browsers use, when one is configured.
func (s *Server) dashboardHost() string {
	for _, env := range []string{envDashboardHost, "GATEWAY_DOMAIN"} {
		if v := hostOnly(os.Getenv(env)); v != "" && v != "gateway.invalid" {
			return v
		}
	}
	return ""
}

// hostOnly strips any port and lower-cases a Host header value.
func hostOnly(host string) string {
	host = strings.TrimSpace(host)
	if h, _, found := strings.Cut(host, ":"); found {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// authExemptPath lists what an unauthenticated request may still reach. It is
// an allowlist by construction: a route added later is protected by default.
func authExemptPath(path string) bool {
	switch {
	case path == "/auth" || strings.HasPrefix(path, "/auth/"):
		return true
	case path == "/health" || path == "/healthz" || path == "/api/v1/health":
		// Health is how the host, the front proxy and the CLI decide the
		// gateway is up; it exposes no configuration and must not need a login.
		return true
	case path == "/ca.crt" || path == "/api/v1/ca.pem":
		// The gateway's CA is public by design: every client needs it to
		// verify this server, so a worker enrolling on a fresh machine must be
		// able to fetch it before it has any credential. The worker mux serves
		// /ca.crt and the dashboard mux serves /api/v1/ca.pem; both must work
		// unauthenticated or the bootstrap deadlocks.
		return true
	case path == "/favicon.ico":
		return true
	}
	return false
}

// sameOrigin refuses cross-site unsafe requests. Cookies make CSRF real, and
// nothing in the gateway checked Origin before this.
//
// SameSite=Lax already keeps the cookie off cross-site POSTs; this closes the
// older-browser gap and the "cookie still sent because the request is
// same-site-but-cross-origin" case. Non-browser clients (the CLI, curl) send
// neither header and are allowed through — they must present a session first.
func sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		if site != "same-origin" && site != "none" {
			return false
		}
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	}
	return true
}

// wantsJSON decides between a 401 JSON body and the login page.
func wantsJSON(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		return true
	}
	return r.Header.Get("X-Requested-With") != ""
}

// dashboardAuth wraps the dashboard mux (HTML + /api/v1) with the session gate.
func (s *Server) dashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		required, reason := s.authRequiredFor(r)
		if !required || authExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
			return
		}
		// A request signed by the host identity key *is* root on this host, so
		// it stands in for a session. That keeps the CLI's administrative calls
		// working even when the gate is forced on.
		if !s.hostAuthed(r) {
			sess, ok := s.readSession(r)
			if !ok {
				w.Header().Set("X-Piso-Auth", reason)
				w.Header().Set("Cache-Control", "no-store")
				if wantsJSON(r) {
					writeJSON(w, http.StatusUnauthorized, map[string]string{
						"error": "authentication required",
						"login": "/auth/link",
					})
					return
				}
				s.serveLoginPage(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
			return
		}
		next.ServeHTTP(w, r.WithContext(withHostAuth(r.Context())))
	})
}

// --- sessions ---------------------------------------------------------------

// identity loads the gateway's identity (the signing key is the session key's
// root, so a session survives rebuilds and `piso update`).
func (s *Server) identity() (tc.Identity, error) {
	if strings.TrimSpace(s.DataDir) == "" {
		return tc.Identity{}, fmt.Errorf("no data dir")
	}
	return tc.Load(tc.Path(s.DataDir))
}

// sessionKey derives the cookie MAC key from the identity signing seed. This is
// a single-purpose derivation (one purpose, one label), which is all that is
// needed here — the seed never leaves the host and has no other exposed use.
func (s *Server) sessionKey() ([]byte, error) {
	id, err := s.identity()
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(id.SigningPrivateKey))
	if err != nil || len(seed) == 0 {
		return nil, fmt.Errorf("identity signing key: unusable")
	}
	mac := hmac.New(sha256.New, seed)
	mac.Write([]byte("piso-dashboard-session-v1"))
	return mac.Sum(nil), nil
}

// seal signs an arbitrary payload for a cookie.
func (s *Server) seal(kind string, payload any) (string, error) {
	key, err := s.sessionKey()
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// open verifies and decodes a sealed cookie value into dst.
func (s *Server) open(kind, value string, dst any) error {
	key, err := s.sessionKey()
	if err != nil {
		return err
	}
	bodyB64, sigB64, ok := strings.Cut(value, ".")
	if !ok {
		return fmt.Errorf("malformed cookie")
	}
	body, err := base64.RawURLEncoding.DecodeString(bodyB64)
	if err != nil {
		return fmt.Errorf("malformed cookie")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("malformed cookie")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return fmt.Errorf("cookie signature mismatch")
	}
	return json.Unmarshal(body, dst)
}

// readSession returns the request's session when it verifies, has not expired,
// and was issued before the last logout-all.
func (s *Server) readSession(r *http.Request) (dashSession, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return dashSession{}, false
	}
	var sess dashSession
	if err := s.open("session", c.Value, &sess); err != nil {
		return dashSession{}, false
	}
	now := time.Now()
	if sess.expired(now) || sess.Epoch != s.Store.SessionEpoch() {
		return dashSession{}, false
	}
	// The credential may have been removed since the cookie was issued.
	if sess.Cred != "" {
		if _, ok := s.Store.CredentialByID(sess.Cred); !ok {
			return dashSession{}, false
		}
	}
	return sess, true
}

// issueSession sets the session cookie for a freshly authenticated request.
func (s *Server) issueSession(w http.ResponseWriter, credID, kind string) {
	now := time.Now()
	sess := dashSession{
		Cred:  credID,
		Kind:  kind,
		Epoch: s.Store.SessionEpoch(),
		Iat:   now.Unix(),
		Exp:   now.Add(sessionTTL).Unix(),
	}
	value, err := s.seal("session", sess)
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// clearSession drops the session cookie.
func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// --- ceremony state ---------------------------------------------------------

// ceremony is the short-lived state between the two halves of a WebAuthn
// ceremony. It is sealed into a cookie so the server keeps nothing per browser.
type ceremony struct {
	Purpose string          `json:"p"`
	Link    enrollLink      `json:"l,omitempty"`
	Data    json.RawMessage `json:"d,omitempty"`
	Exp     int64           `json:"x"`
}

type enrollLink struct {
	Nonce string `json:"n"`
	Exp   int64  `json:"e"`
	Sig   string `json:"s"`
}

func (l enrollLink) empty() bool { return l.Nonce == "" && l.Sig == "" }

func (s *Server) setCeremony(w http.ResponseWriter, c ceremony) error {
	c.Exp = time.Now().Add(ceremonyTTL).Unix()
	value, err := s.seal("ceremony", c)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     ceremonyCookie,
		Value:    value,
		Path:     "/auth/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ceremonyTTL.Seconds()),
	})
	return nil
}

func (s *Server) takeCeremony(w http.ResponseWriter, r *http.Request, purpose string) (ceremony, bool) {
	c, err := r.Cookie(ceremonyCookie)
	if err != nil || c.Value == "" {
		return ceremony{}, false
	}
	var ce ceremony
	if err := s.open("ceremony", c.Value, &ce); err != nil {
		return ceremony{}, false
	}
	// Single use: clear before the caller acts on it.
	http.SetCookie(w, &http.Cookie{
		Name: ceremonyCookie, Value: "", Path: "/auth/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	if ce.Purpose != purpose || time.Now().Unix() >= ce.Exp {
		return ceremony{}, false
	}
	return ce, true
}

// --- enrollment links -------------------------------------------------------

// verifyEnrollLink checks a link against this gateway's own identity key and
// consumes its nonce, so a link works exactly once.
func (s *Server) verifyEnrollLink(link enrollLink) error {
	id, err := s.identity()
	if err != nil {
		return fmt.Errorf("identity unavailable: %w", err)
	}
	if err := authlink.Verify(id.SigningPublicKey, link.Nonce, link.Sig, link.Exp, time.Now()); err != nil {
		return err
	}
	if !s.consumeNonce("enroll:"+link.Nonce, time.Unix(link.Exp, 0)) {
		return fmt.Errorf("this enrollment link was already used")
	}
	return nil
}

// nonceCache is a small TTL map for single-use values (enrollment links and
// host-auth request nonces). It is in memory on purpose: both are short-lived,
// and a gateway restart re-arming a link for the remainder of its ten minutes is
// not a meaningful window.
type nonceCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (c *nonceCache) add(key string, until time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]time.Time{}
	}
	now := time.Now()
	for k, exp := range c.seen {
		if now.After(exp) {
			delete(c.seen, k)
		}
	}
	if exp, ok := c.seen[key]; ok && now.Before(exp) {
		return false
	}
	c.seen[key] = until.Add(2 * time.Minute)
	return true
}

func (s *Server) consumeNonce(key string, until time.Time) bool {
	return s.authNonces.add(key, until)
}

// --- host authentication ----------------------------------------------------

// hostAuthed reports whether the request carries a valid signature from the
// host's identity key. Used only by the administrative endpoints below.
func (s *Server) hostAuthed(r *http.Request) bool {
	id, err := s.identity()
	if err != nil {
		return false
	}
	if err := hostauth.Verify(id.SigningPublicKey, r, r.URL.RequestURI(), time.Now()); err != nil {
		return false
	}
	until := time.Now().Add(hostauth.MaxSkew)
	return s.consumeNonce(hostauth.NonceKey(r.Header.Get(hostauth.HeaderNonce)), until)
}

// requireHost guards the administrative auth endpoints.
func (s *Server) requireHost(w http.ResponseWriter, r *http.Request) bool {
	if hostAuthedFromContext(r.Context()) || s.hostAuthed(r) {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error": "this endpoint requires a request signed by the gateway host identity key",
	})
	return false
}

// --- HTTP surface -----------------------------------------------------------

// authRoutes registers the dashboard-auth API on the control mux.
func (s *Server) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/auth/state", s.handleAuthState)
	mux.HandleFunc("POST /api/v1/auth/logout-all", s.handleAuthLogoutAll)
	// A fleet path: add a nostr key from the host, no browser. A passkey cannot
	// be added this way — its key material is generated inside the authenticator
	// and never leaves it.
	mux.HandleFunc("POST /api/v1/auth/nostr-keys", s.handleAddNostrKey)
	mux.HandleFunc("DELETE /api/v1/auth/credentials/{id}", s.handleAuthDeleteCredential)
}

func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	if !s.requireHost(w, r) {
		return
	}
	required, reason := s.dashboardPolicy()
	creds := s.Store.CredentialRecs()
	out := make([]map[string]any, 0, len(creds))
	for _, c := range creds {
		out = append(out, map[string]any{
			"id":        c.ID,
			"kind":      c.Kind,
			"label":     c.Label,
			"pubkey":    c.Pubkey, // nostr only; a passkey's key material lives in its authenticator
			"createdAt": c.CreatedAt,
			"lastUsed":  c.LastUsed,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"required":    required,
		"reason":      reason,
		"host":        s.dashboardHost(),
		"epoch":       s.Store.SessionEpoch(),
		"credentials": out,
	})
}

func (s *Server) handleAuthLogoutAll(w http.ResponseWriter, r *http.Request) {
	if !s.requireHost(w, r) {
		return
	}
	epoch, err := s.Store.BumpSessionEpoch()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"epoch": epoch, "sessions": "all revoked"})
}

func (s *Server) handleAuthDeleteCredential(w http.ResponseWriter, r *http.Request) {
	if !s.requireHost(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := s.Store.DeleteCredential(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// authPublicRoutes registers the endpoints a browser reaches without a session.
func (s *Server) authPublicRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/link", s.handleEnrollPage)
	mux.HandleFunc("GET /auth/session", s.handleAuthSessionInfo)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)

	mux.HandleFunc("POST /auth/passkey/login/begin", s.handlePasskeyLoginBegin)
	mux.HandleFunc("POST /auth/passkey/login/finish", s.handlePasskeyLoginFinish)
	mux.HandleFunc("POST /auth/passkey/register/begin", s.handlePasskeyRegisterBegin)
	mux.HandleFunc("POST /auth/passkey/register/finish", s.handlePasskeyRegisterFinish)

	mux.HandleFunc("GET /auth/nostr/challenge", s.handleNostrChallenge)
	mux.HandleFunc("POST /auth/nostr/verify", s.handleNostrVerify)
	mux.HandleFunc("POST /auth/nostr/enroll", s.handleNostrEnroll)
}

func (s *Server) handleAuthSessionInfo(w http.ResponseWriter, r *http.Request) {
	required, reason := s.dashboardPolicy()
	sess, ok := s.readSession(r)
	out := map[string]any{"required": required, "reason": reason, "authenticated": ok}
	if ok {
		out["kind"] = sess.Kind
		out["expires"] = sess.Exp
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
		return
	}
	s.clearSession(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// decodeEnrollLink pulls the link out of a query string or a JSON body.
func decodeEnrollLink(q url.Values) enrollLink {
	link := enrollLink{
		Nonce: q.Get("n"),
		Sig:   q.Get("s"),
	}
	if e := q.Get("e"); e != "" {
		var exp int64
		if _, err := fmt.Sscanf(e, "%d", &exp); err == nil {
			link.Exp = exp
		}
	}
	return link
}

// verifyLinkAt checks a link's signature and expiry with an injected clock.
func verifyLinkAt(pubHex string, link enrollLink, now time.Time) error {
	return authlink.Verify(pubHex, link.Nonce, link.Sig, link.Exp, now)
}
