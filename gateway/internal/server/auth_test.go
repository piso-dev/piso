package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"piso/gateway/internal/store"
	"piso/internal/authlink"
	"piso/internal/hostauth"
	"piso/internal/technocore"
)

// testAuthServer is testServer plus a persisted identity, which the session key
// and every signature check is derived from.
func testAuthServer(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	dir := t.TempDir()
	id, err := technocore.LoadOrCreate(technocore.Path(dir), "https://server.invalid", "test-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if err := technocore.Save(technocore.Path(dir), id); err != nil {
		t.Fatal(err)
	}
	s.DataDir = dir
	return s
}

func (s *Server) testIdentity(t *testing.T) technocore.Identity {
	t.Helper()
	id, err := technocore.Load(technocore.Path(s.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAuthExemptPathIsAllowlist(t *testing.T) {
	exempt := []string{"/auth/link", "/auth", "/auth/nostr/challenge", "/health", "/healthz", "/ca.crt", "/favicon.ico"}
	for _, p := range exempt {
		if !authExemptPath(p) {
			t.Fatalf("%s must be reachable before login", p)
		}
	}
	// Every control-plane route, and anything added later, must be gated.
	for _, p := range []string{
		"/", "/api/v1/workers", "/api/v1/activities", "/api/v1/model-overrides",
		"/api/v1/connections", "/api/v1/auth/state", "/some/new/route", "/authentication",
	} {
		if authExemptPath(p) {
			t.Fatalf("%s must require a session", p)
		}
	}
}

func TestSameOriginBlocksCrossSiteWrites(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		origin  string
		fetchSf string
		want    bool
	}{
		{"safe method ignores origin", http.MethodGet, "https://evil.example", "", true},
		{"same origin write", http.MethodPost, "https://dash.example", "", true},
		{"cross origin write", http.MethodPost, "https://evil.example", "", false},
		{"cross site fetch metadata", http.MethodPost, "", "cross-site", false},
		{"same-origin fetch metadata", http.MethodPost, "", "same-origin", true},
		{"user-typed navigation", http.MethodPost, "", "none", true},
		{"no headers, non-browser client", http.MethodDelete, "", "", true},
		{"garbage origin", http.MethodPost, "::::", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "https://dash.example/api/v1/workers", nil)
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.fetchSf != "" {
				r.Header.Set("Sec-Fetch-Site", c.fetchSf)
			}
			if got := sameOrigin(r); got != c.want {
				t.Fatalf("sameOrigin = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSessionCookieLifecycle(t *testing.T) {
	s := testAuthServer(t)
	rec := addTestCred(t, s, "test")

	issue := func() *http.Cookie {
		w := httptest.NewRecorder()
		s.issueSession(w, rec.ID, store.CredentialPasskey)
		for _, c := range w.Result().Cookies() {
			if c.Name == sessionCookie {
				return c
			}
		}
		t.Fatal("no session cookie issued")
		return nil
	}

	t.Run("valid", func(t *testing.T) {
		c := issue()
		if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
			t.Fatalf("cookie flags: httponly=%v secure=%v samesite=%v", c.HttpOnly, c.Secure, c.SameSite)
		}
		r := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
		r.AddCookie(c)
		sess, ok := s.readSession(r)
		if !ok || sess.Cred != rec.ID {
			t.Fatalf("valid cookie rejected: ok=%v sess=%+v", ok, sess)
		}
	})

	t.Run("absent", func(t *testing.T) {
		if _, ok := s.readSession(httptest.NewRequest(http.MethodGet, "/", nil)); ok {
			t.Fatal("no cookie must not authenticate")
		}
	})

	t.Run("forged payload keeps a valid mac", func(t *testing.T) {
		c := issue()
		// Swap the payload for another credential id; the MAC no longer matches.
		body, sig, _ := strings.Cut(c.Value, ".")
		other, _ := json.Marshal(dashSession{Cred: "someone-else", Kind: store.CredentialPasskey, Exp: time.Now().Add(time.Hour).Unix()})
		forged := base64.RawURLEncoding.EncodeToString(other) + "." + sig
		_ = body
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: forged})
		if _, ok := s.readSession(r); ok {
			t.Fatal("tampered payload must not authenticate")
		}
	})

	t.Run("signature tampered", func(t *testing.T) {
		c := issue()
		body, sig, _ := strings.Cut(c.Value, ".")
		bad := sig
		if len(bad) > 2 {
			bad = sig[:len(sig)-2] + "AA"
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: body + "." + bad})
		if _, ok := s.readSession(r); ok {
			t.Fatal("tampered signature must not authenticate")
		}
	})

	t.Run("expired", func(t *testing.T) {
		value, err := s.seal("session", dashSession{
			Cred: rec.ID, Kind: store.CredentialPasskey,
			Epoch: s.Store.SessionEpoch(), Exp: time.Now().Add(-time.Minute).Unix(),
		})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: value})
		if _, ok := s.readSession(r); ok {
			t.Fatal("expired session must not authenticate")
		}
	})

	t.Run("epoch revoked", func(t *testing.T) {
		c := issue()
		if _, err := s.Store.BumpSessionEpoch(); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(c)
		if _, ok := s.readSession(r); ok {
			t.Fatal("logout-all must invalidate existing sessions")
		}
	})

	t.Run("credential removed", func(t *testing.T) {
		rec2 := addTestCred(t, s, "second")
		w := httptest.NewRecorder()
		s.issueSession(w, rec2.ID, store.CredentialPasskey)
		var c *http.Cookie
		for _, got := range w.Result().Cookies() {
			if got.Name == sessionCookie {
				c = got
			}
		}
		if c == nil {
			t.Fatal("no cookie")
		}
		if err := s.Store.DeleteCredential(rec2.ID); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(c)
		if _, ok := s.readSession(r); ok {
			t.Fatal("a deleted credential's session must stop working")
		}
	})
}

// A session signed by a *different* gateway must not be accepted: the key is
// derived from the identity, not from a constant.
func TestSessionKeyIsPerGateway(t *testing.T) {
	a := testAuthServer(t)
	b := testAuthServer(t)
	if string(mustKey(t, a)) == string(mustKey(t, b)) {
		t.Fatal("two gateways must not share a session key")
	}
	value, err := a.seal("session", dashSession{Kind: store.CredentialPasskey, Exp: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: value})
	if _, ok := b.readSession(r); ok {
		t.Fatal("another gateway's session must not be accepted")
	}
}

func mustKey(t *testing.T, s *Server) []byte {
	t.Helper()
	k, err := s.sessionKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The gate must leave a local gateway alone, and must stop an off-host request
// that carries no credential.
func TestDashboardAuthGate(t *testing.T) {
	const proxyPeer = "172.30.99.5:41000"

	render := func(s *Server, path, host, remote string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
		r.RemoteAddr = remote
		s.WebHandler().ServeHTTP(w, r)
		return w
	}

	t.Run("local gateway is unaffected", func(t *testing.T) {
		s := testAuthServer(t)
		t.Setenv("PISO_DASHBOARD_AUTH", "")
		t.Setenv("PISO_DASHBOARD_HOST", "")
		w := render(s, "/api/v1/workers", "127.0.0.1:8080", "127.0.0.1:5555")
		if w.Code != http.StatusOK {
			t.Fatalf("loopback request must not need a login, got %d: %s", w.Code, w.Body)
		}
	})

	t.Run("off-host through the proxy is gated", func(t *testing.T) {
		s := testAuthServer(t)
		t.Setenv("PISO_TRUSTED_PEER_CIDRS", "172.30.99.0/24")
		t.Setenv("PISO_DASHBOARD_AUTH", "")
		t.Setenv("PISO_DASHBOARD_HOST", "")
		w := render(s, "/api/v1/workers", "dash.example", proxyPeer)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("proxied request must need a session, got %d: %s", w.Code, w.Body)
		}
		if got := w.Header().Get("X-Piso-Auth"); got == "" {
			t.Fatal("the 401 must say which rule fired")
		}
		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("401 body must be JSON: %v (%s)", err, w.Body)
		}
		if body["error"] == "" {
			t.Fatal("401 body must explain itself")
		}
	})

	t.Run("public dashboard host is gated", func(t *testing.T) {
		s := testAuthServer(t)
		t.Setenv("PISO_TRUSTED_PEER_CIDRS", "")
		t.Setenv("PISO_DASHBOARD_AUTH", "")
		t.Setenv("PISO_DASHBOARD_HOST", "dash.example")
		// Not a trusted peer: the rule that fires is the hostname, not the peer.
		w := render(s, "/api/v1/workers", "dash.example", "203.0.113.9:1234")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("dashboard host must need a session, got %d", w.Code)
		}
	})

	t.Run("browser navigation gets a page, not JSON", func(t *testing.T) {
		s := testAuthServer(t)
		t.Setenv("PISO_DASHBOARD_AUTH", "on")
		w := render(s, "/", "dash.example", proxyPeer)
		if w.Code != http.StatusOK {
			t.Fatalf("login page status = %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("want the login page, got %s", ct)
		}
		if !strings.Contains(w.Body.String(), "passkey") {
			t.Fatal("login page must offer a passkey")
		}
	})

	// The machine planes must never enter the gate: a worker's request to a
	// routed hostname is proxied, not challenged, even from off-host.
	t.Run("worker route is not gated", func(t *testing.T) {
		s := testAuthServer(t)
		t.Setenv("PISO_DASHBOARD_AUTH", "on")
		t.Setenv("PISO_DASHBOARD_HOST", "dash.example")
		w := render(s, "/", "demo.piso.local", "203.0.113.9:1234")
		if w.Code == http.StatusUnauthorized {
			t.Fatalf("worker-plane traffic must not hit the dashboard gate: %s", w.Body)
		}
	})

	t.Run("health and ca.crt stay public", func(t *testing.T) {
		s := testAuthServer(t)
		t.Setenv("PISO_DASHBOARD_AUTH", "on")
		if w := render(s, "/api/v1/health", "dash.example", proxyPeer); w.Code != http.StatusOK {
			t.Fatalf("/api/v1/health must stay public, got %d", w.Code)
		}
		// Whether /health is routed is a UI concern; it must never be a 401.
		if w := render(s, "/health", "dash.example", proxyPeer); w.Code == http.StatusUnauthorized {
			t.Fatal("/health must not be gated")
		}
	})

	t.Run("env can force it off", func(t *testing.T) {
		s := testAuthServer(t)
		t.Setenv("PISO_DASHBOARD_HOST", "dash.example")
		t.Setenv("PISO_DASHBOARD_AUTH", "off")
		if w := render(s, "/api/v1/workers", "dash.example", proxyPeer); w.Code != http.StatusOK {
			t.Fatalf("PISO_DASHBOARD_AUTH=off must win, got %d", w.Code)
		}
	})
}

func TestEnrollLinkIsSingleUseAndBound(t *testing.T) {
	s := testAuthServer(t)
	id := s.testIdentity(t)
	nonce, err := authlink.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(authlink.TTL).Unix()
	sig, err := authlink.Sign(id.SigningPrivateKey, nonce, exp)
	if err != nil {
		t.Fatal(err)
	}
	link := enrollLink{Nonce: nonce, Exp: exp, Sig: sig}

	if err := s.verifyEnrollLink(link); err != nil {
		t.Fatalf("fresh link rejected: %v", err)
	}
	if err := s.verifyEnrollLink(link); err == nil {
		t.Fatal("a link must work exactly once")
	}

	// A link signed by another host's key must never verify.
	other := testAuthServer(t)
	oid := other.testIdentity(t)
	nonce2, err := authlink.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	sig2, err := authlink.Sign(oid.SigningPrivateKey, nonce2, exp)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.verifyEnrollLink(enrollLink{Nonce: nonce2, Exp: exp, Sig: sig2}); err == nil {
		t.Fatal("a foreign signature must not enroll")
	}

	// Expired.
	nonce3, err := authlink.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute).Unix()
	sig3, err := authlink.Sign(id.SigningPrivateKey, nonce3, past)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.verifyEnrollLink(enrollLink{Nonce: nonce3, Exp: past, Sig: sig3}); err == nil {
		t.Fatal("an expired link must not enroll")
	}
}

func TestEnrollPageRequiresLink(t *testing.T) {
	s := testAuthServer(t)
	t.Setenv("PISO_DASHBOARD_AUTH", "on")

	get := func(query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "https://dash.example/auth/link"+query, nil)
		s.WebHandler().ServeHTTP(w, r)
		return w
	}

	if w := get(""); !strings.Contains(w.Body.String(), "Link not valid") {
		t.Fatalf("a bare /auth/link must not offer registration: %s", w.Body)
	}
	if w := get("?n=abc&e=9999999999&s=deadbeef"); !strings.Contains(w.Body.String(), "Link not valid") {
		t.Fatalf("a forged link must not offer registration: %s", w.Body)
	}

	id := s.testIdentity(t)
	nonce, err := authlink.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(authlink.TTL).Unix()
	sig, err := authlink.Sign(id.SigningPrivateKey, nonce, exp)
	if err != nil {
		t.Fatal(err)
	}
	w := get(authlink.Query(nonce, sig, exp))
	if !strings.Contains(w.Body.String(), "Register a passkey") {
		t.Fatalf("a valid link must offer registration: %s", w.Body)
	}
}

// --- host authentication ----------------------------------------------------

func TestHostAuthRequiredForAdminEndpoints(t *testing.T) {
	s := testAuthServer(t)
	id := s.testIdentity(t)

	call := func(signed bool, path, method string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, nil)
		r.RemoteAddr = "127.0.0.1:5555"
		if signed {
			if err := hostauth.Apply(r, id.SigningPrivateKey, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		s.WebHandler().ServeHTTP(w, r)
		return w
	}

	if w := call(false, "/api/v1/auth/state", http.MethodGet); w.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned admin call must be refused, got %d: %s", w.Code, w.Body)
	}
	w := call(true, "/api/v1/auth/state", http.MethodGet)
	if w.Code != http.StatusOK {
		t.Fatalf("signed admin call must work, got %d: %s", w.Code, w.Body)
	}
	var st map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if _, ok := st["credentials"]; !ok {
		t.Fatalf("state must list credentials: %s", w.Body)
	}

	// Replay of the same signature must fail: nonces are consumed.
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/v1/auth/state", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	if err := hostauth.Apply(r, id.SigningPrivateKey, time.Now()); err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	s.WebHandler().ServeHTTP(first, r)
	second := httptest.NewRecorder()
	s.WebHandler().ServeHTTP(second, r)
	if first.Code != http.StatusOK || second.Code != http.StatusUnauthorized {
		t.Fatalf("host signatures must be single use: first=%d second=%d", first.Code, second.Code)
	}
}

// A host-signed request stands in for a session, so forcing the gate on does
// not lock the CLI out of its own administrative endpoints.
func TestHostSignaturePassesGate(t *testing.T) {
	s := testAuthServer(t)
	id := s.testIdentity(t)
	t.Setenv("PISO_DASHBOARD_AUTH", "on")

	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/v1/auth/state", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	if err := hostauth.Apply(r, id.SigningPrivateKey, time.Now()); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.WebHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("signed request must pass the forced gate, got %d: %s", w.Code, w.Body)
	}
}

// --- nostr ------------------------------------------------------------------

// signNostr builds a NIP-01 event signed with the given key.
func signNostr(t *testing.T, priv *btcec.PrivateKey, createdAt int64, kind int, tags [][]string, content string) nostrEvent {
	t.Helper()
	pub := hex.EncodeToString(schnorr.SerializePubKey(priv.PubKey()))
	id, err := nostrEventID(pub, createdAt, kind, tags, content)
	if err != nil {
		t.Fatal(err)
	}
	rawID, err := hex.DecodeString(id)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := schnorr.Sign(priv, rawID)
	if err != nil {
		t.Fatal(err)
	}
	return nostrEvent{
		ID: id, PubKey: pub, CreatedAt: createdAt, Kind: kind,
		Tags: tags, Content: content, Sig: hex.EncodeToString(sig.Serialize()),
	}
}

func TestNostrEventVerification(t *testing.T) {
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ch := nostrChallenge{Host: "dash.example", Purpose: "login", Exp: now.Add(5 * time.Minute).Unix()}
	good := signNostr(t, priv, now.Unix(), nostrKindAuth, [][]string{
		{"challenge", "abc"}, {"domain", "dash.example"},
	}, "")

	if err := verifyNostrEvent(good, ch, now); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}

	t.Run("tampered content", func(t *testing.T) {
		bad := good
		bad.Content = "x"
		if err := verifyNostrEvent(bad, ch, now); err == nil {
			t.Fatal("an event whose id does not match its content must fail")
		}
	})
	t.Run("wrong kind", func(t *testing.T) {
		bad := signNostr(t, priv, now.Unix(), 1, [][]string{{"challenge", "abc"}, {"domain", "dash.example"}}, "")
		if err := verifyNostrEvent(bad, ch, now); err == nil {
			t.Fatal("only the auth kind may sign in")
		}
	})
	t.Run("stale", func(t *testing.T) {
		bad := signNostr(t, priv, now.Add(-time.Hour).Unix(), nostrKindAuth, [][]string{{"challenge", "abc"}, {"domain", "dash.example"}}, "")
		if err := verifyNostrEvent(bad, ch, now); err == nil {
			t.Fatal("a stale event must not sign in")
		}
	})
	t.Run("wrong domain", func(t *testing.T) {
		bad := signNostr(t, priv, now.Unix(), nostrKindAuth, [][]string{{"challenge", "abc"}, {"domain", "other.example"}}, "")
		if err := verifyNostrEvent(bad, ch, now); err == nil {
			t.Fatal("the domain tag must match the challenge's host")
		}
	})
	t.Run("missing domain", func(t *testing.T) {
		bad := signNostr(t, priv, now.Unix(), nostrKindAuth, [][]string{{"challenge", "abc"}}, "")
		if err := verifyNostrEvent(bad, ch, now); err == nil {
			t.Fatal("the domain tag is required")
		}
	})
	t.Run("foreign signer", func(t *testing.T) {
		other, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		bad := good
		bad.PubKey = hex.EncodeToString(schnorr.SerializePubKey(other.PubKey()))
		if err := verifyNostrEvent(bad, ch, now); err == nil {
			t.Fatal("a swapped pubkey must fail")
		}
	})
}

func TestNostrEnrollThenLogin(t *testing.T) {
	s := testAuthServer(t)
	id := s.testIdentity(t)
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()

	newChallenge := func(purpose string) string {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "https://dash.example/auth/nostr/challenge?purpose="+purpose, nil)
		r.Host = "dash.example"
		s.handleNostrChallenge(w, r)
		var out struct {
			Challenge string `json:"challenge"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("challenge: %v (%s)", err, w.Body)
		}
		return out.Challenge
	}
	post := func(path string, body any) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "https://dash.example"+path, strings.NewReader(string(raw)))
		r.Host = "dash.example"
		r.Header.Set("Content-Type", "application/json")
		s.authPublicRoutesForTest().ServeHTTP(w, r)
		return w
	}

	nonce, err := authlink.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(authlink.TTL).Unix()
	sig, err := authlink.Sign(id.SigningPrivateKey, nonce, exp)
	if err != nil {
		t.Fatal(err)
	}

	enrollChallenge := newChallenge("enroll")
	ev := signNostr(t, priv, now, nostrKindAuth, [][]string{
		{"challenge", enrollChallenge}, {"domain", "dash.example"},
	}, "")
	w := post("/auth/nostr/enroll", map[string]any{"event": ev, "n": nonce, "e": exp, "s": sig, "label": "laptop"})
	if w.Code != http.StatusOK {
		t.Fatalf("enroll failed: %d %s", w.Code, w.Body)
	}
	if len(s.Store.CredentialRecs()) != 1 {
		t.Fatalf("want one credential, got %d", len(s.Store.CredentialRecs()))
	}

	// The same challenge must not be reusable.
	if w := post("/auth/nostr/enroll", map[string]any{"event": ev, "n": nonce, "e": exp, "s": sig}); w.Code == http.StatusOK {
		t.Fatal("a challenge must be single use")
	}

	// Login with a fresh challenge now succeeds and issues a session.
	loginChallenge := newChallenge("login")
	ev = signNostr(t, priv, now, nostrKindAuth, [][]string{
		{"challenge", loginChallenge}, {"domain", "dash.example"},
	}, "")
	w = post("/auth/nostr/verify", map[string]any{"event": ev})
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", w.Code, w.Body)
	}
	var gotSession bool
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			gotSession = true
		}
	}
	if !gotSession {
		t.Fatal("a successful sign-in must issue a session cookie")
	}

	// An unregistered key is refused, and never creates a credential.
	other, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	loginChallenge = newChallenge("login")
	ev = signNostr(t, other, now, nostrKindAuth, [][]string{
		{"challenge", loginChallenge}, {"domain", "dash.example"},
	}, "")
	if w := post("/auth/nostr/verify", map[string]any{"event": ev}); w.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown key must be refused, got %d: %s", w.Code, w.Body)
	}
}

// authPublicRoutesForTest is the public auth mux alone, so a device test does
// not have to go through the whole web dispatcher.
func (s *Server) authPublicRoutesForTest() http.Handler {
	mux := http.NewServeMux()
	s.authPublicRoutes(mux)
	return mux
}

func TestPasskeyNeedsDNSName(t *testing.T) {
	s := testAuthServer(t)
	r := httptest.NewRequest(http.MethodPost, "https://203.0.113.10/auth/passkey/login/begin", nil)
	r.Host = "203.0.113.10"
	if _, err := s.passkeyRelyingParty(r); err == nil {
		t.Fatal("passkeys must refuse an IP address as the relying party")
	}
}

func TestEnrollLinkURLRoundTrip(t *testing.T) {
	nonce, err := authlink.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	link := authlink.URL("https://dash.example", nonce, "sig==", 1700000000)
	if !strings.HasPrefix(link, "https://dash.example/auth/link?") {
		t.Fatalf("link shape: %s", link)
	}
	q := strings.SplitN(link, "?", 2)[1]
	values := parseQuery(t, q)
	got := decodeEnrollLink(values)
	if got.Nonce != nonce || got.Sig != "sig==" || got.Exp != 1700000000 {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

func parseQuery(t *testing.T, raw string) map[string][]string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/auth/link?"+raw, nil)
	return r.URL.Query()
}

// The dashboard's own CLI-facing endpoints must stay behind the session gate,
// while the machine planes are reachable without one.
func TestGateCoversControlAPI(t *testing.T) {
	s := testAuthServer(t)
	t.Setenv("PISO_DASHBOARD_AUTH", "on")
	for _, path := range []string{
		"/api/v1/workers", "/api/v1/activities", "/api/v1/model-overrides",
		"/api/v1/connections", "/api/v1/secrets", "/api/v1/directives",
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "https://dash.example"+path, nil)
		r.Header.Set("Content-Type", "application/json")
		s.WebHandler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s must be gated, got %d", path, w.Code)
		}
	}
}

func TestSessionSigningKeyNeedsIdentity(t *testing.T) {
	s := testServer(t) // no DataDir: a gateway without identity must not mint sessions
	if _, err := s.sessionKey(); err == nil {
		t.Fatal("a gateway with no identity must not produce a session key")
	}
	w := httptest.NewRecorder()
	s.issueSession(w, "nobody", store.CredentialPasskey)
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("no cookie may be issued without a key")
		}
	}
}

var _ = ed25519.PublicKeySize
var _ = rand.Reader
var _ = os.Getenv
var _ = filepath.Join

// addTestPasskey registers a passkey row. Registration needs a real
// authenticator, so tests build the stored record directly.
func addTestPasskey(t *testing.T, s *Server, label string, credID []byte) store.CredentialRec {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id":              credID,
		"publicKey":       []byte{0x30, 0x59, 0x30},
		"attestationType": "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Store.AddCredential(store.CredentialRec{
		Kind: store.CredentialPasskey, Label: label, Passkey: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// A passkey and a nostr key are two independent credentials on one session
// plane: either signs you in, and removing one leaves the other working.
func TestPasskeyAndNostrCoexist(t *testing.T) {
	s := testAuthServer(t)
	pass := addTestPasskey(t, s, "laptop", []byte{0x01, 0x02, 0x03, 0x04})
	nostr := addTestCred(t, s, "phone")

	recs := s.Store.CredentialRecs()
	if len(recs) != 2 {
		t.Fatalf("want both credentials kept, got %d", len(recs))
	}
	if pass.ID == nostr.ID {
		t.Fatal("credentials must be distinct rows")
	}

	// The passkey user carries only passkeys; the nostr row never leaks into a
	// WebAuthn ceremony.
	user, err := s.webauthnUser()
	if err != nil {
		t.Fatal(err)
	}
	if len(user.creds) != 1 {
		t.Fatalf("webauthn user must hold only the passkey, got %d", len(user.creds))
	}

	// Either credential opens a session of its own kind.
	for _, c := range []struct{ rec store.CredentialRec }{{pass}, {nostr}} {
		w := httptest.NewRecorder()
		s.issueSession(w, c.rec.ID, c.rec.Kind)
		var cookie *http.Cookie
		for _, got := range w.Result().Cookies() {
			if got.Name == sessionCookie {
				cookie = got
			}
		}
		if cookie == nil {
			t.Fatalf("%s: no session cookie", c.rec.Kind)
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(cookie)
		sess, ok := s.readSession(r)
		if !ok || sess.Kind != c.rec.Kind || sess.Cred != c.rec.ID {
			t.Fatalf("%s: session not bound to its credential: ok=%v sess=%+v", c.rec.Kind, ok, sess)
		}
	}

	// Re-registering the same authenticator replaces its row instead of
	// duplicating it, and keeps the row id sessions point at.
	again := addTestPasskey(t, s, "laptop (again)", []byte{0x01, 0x02, 0x03, 0x04})
	if again.ID != pass.ID {
		t.Fatalf("same authenticator must replace its row: %s != %s", again.ID, pass.ID)
	}
	if again.Label != "laptop (again)" {
		t.Fatalf("re-registration must update the label, got %q", again.Label)
	}
	if got := len(s.Store.CredentialRecs()); got != 2 {
		t.Fatalf("want 2 credentials after re-registering, got %d", got)
	}

	// Removing one must not disturb the other.
	if err := s.Store.DeleteCredential(pass.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Store.CredentialByID(nostr.ID); !ok {
		t.Fatal("deleting the passkey must not remove the nostr key")
	}
	if got := len(s.Store.CredentialRecs()); got != 1 {
		t.Fatalf("want 1 credential left, got %d", got)
	}
}

// addTestCred registers a nostr credential (the store rejects an empty passkey
// row, and the session plane does not care which kind issued it).
func addTestCred(t *testing.T, s *Server, label string) store.CredentialRec {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Store.AddCredential(store.CredentialRec{
		Kind:   store.CredentialNostr,
		Label:  label,
		Pubkey: hex.EncodeToString(schnorr.SerializePubKey(priv.PubKey())),
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// A nostr key is 32 public bytes with no counter and no origin binding, so the
// same key can be an operator on several gateways. Sharing the *key* must not
// share an *authentication event*, though: each gateway mints its own
// single-use challenge and requires the event's domain tag to name it.
func TestNostrKeyAcrossGateways(t *testing.T) {
	t.Setenv("PISO_DASHBOARD_HOST", "") // per-request Host decides the binding

	a := testAuthServer(t)
	b := testAuthServer(t)
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub := hex.EncodeToString(schnorr.SerializePubKey(priv.PubKey()))

	// The same key is enrolled on both gateways, each with its own host-signed
	// link: the key is shareable, the authorisation is not.
	enrollOn := func(s *Server, host string) {
		t.Helper()
		id, err := technocore.Load(technocore.Path(s.DataDir))
		if err != nil {
			t.Fatal(err)
		}
		nonce, err := authlink.NewNonce()
		if err != nil {
			t.Fatal(err)
		}
		exp := time.Now().Add(authlink.TTL).Unix()
		sig, err := authlink.Sign(id.SigningPrivateKey, nonce, exp)
		if err != nil {
			t.Fatal(err)
		}
		ch := challengeFor(t, s, host, "enroll")
		ev := signNostr(t, priv, time.Now().Unix(), nostrKindAuth, [][]string{
			{"challenge", ch}, {"domain", host},
		}, "")
		w := postAuth(t, s, host, "/auth/nostr/enroll", map[string]any{
			"event": ev, "n": nonce, "e": exp, "s": sig, "label": "fleet",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: enroll: %d %s", host, w.Code, w.Body)
		}
	}
	enrollOn(a, "a.example")
	enrollOn(b, "b.example")

	if _, ok := a.Store.CredentialByPubkey(pub); !ok {
		t.Fatal("gateway A must know the key")
	}
	if _, ok := b.Store.CredentialByPubkey(pub); !ok {
		t.Fatal("gateway B must know the same key")
	}

	// It signs in on either one, without any sync between them.
	for _, s := range []*Server{a, b} {
		host := "a.example"
		if s == b {
			host = "b.example"
		}
		ch := challengeFor(t, s, host, "login")
		ev := signNostr(t, priv, time.Now().Unix(), nostrKindAuth, [][]string{
			{"challenge", ch}, {"domain", host},
		}, "")
		w := postAuth(t, s, host, "/auth/nostr/verify", map[string]any{"event": ev})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: login: %d %s", host, w.Code, w.Body)
		}
	}

	t.Run("an event signed for one gateway is not valid at another", func(t *testing.T) {
		// A mints a challenge; the user signs it for A's host.
		ch := challengeFor(t, a, "a.example", "login")
		ev := signNostr(t, priv, time.Now().Unix(), nostrKindAuth, [][]string{
			{"challenge", ch}, {"domain", "a.example"},
		}, "")
		// Replayed at B: B never minted that challenge.
		if w := postAuth(t, b, "b.example", "/auth/nostr/verify", map[string]any{"event": ev}); w.Code != http.StatusBadRequest {
			t.Fatalf("cross-gateway replay must fail, got %d: %s", w.Code, w.Body)
		}
		// And even if both gateways share a hostname, the challenge is not one B
		// minted, so the event still cannot be reused.
		if w := postAuth(t, b, "a.example", "/auth/nostr/verify", map[string]any{"event": ev}); w.Code != http.StatusBadRequest {
			t.Fatalf("replay under a shared hostname must fail, got %d: %s", w.Code, w.Body)
		}
		// The genuine gateway still accepts it (it was never consumed by B).
		if w := postAuth(t, a, "a.example", "/auth/nostr/verify", map[string]any{"event": ev}); w.Code != http.StatusOK {
			t.Fatalf("the minting gateway must accept its own event: %d %s", w.Code, w.Body)
		}
	})

	t.Run("a different key is still refused", func(t *testing.T) {
		other, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		ch := challengeFor(t, b, "b.example", "login")
		ev := signNostr(t, other, time.Now().Unix(), nostrKindAuth, [][]string{
			{"challenge", ch}, {"domain", "b.example"},
		}, "")
		if w := postAuth(t, b, "b.example", "/auth/nostr/verify", map[string]any{"event": ev}); w.Code != http.StatusUnauthorized {
			t.Fatalf("an unknown key must be refused, got %d", w.Code)
		}
	})
}

// challengeFor mints a challenge for one gateway and host pair.
func challengeFor(t *testing.T, s *Server, host, purpose string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "https://"+host+"/auth/nostr/challenge?purpose="+purpose, nil)
	r.Host = host
	w := httptest.NewRecorder()
	s.handleNostrChallenge(w, r)
	var out struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("challenge: %v (%s)", err, w.Body)
	}
	return out.Challenge
}

// postAuth posts to one gateway's public auth surface as a given host.
func postAuth(t *testing.T, s *Server, host, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "https://"+host+path, bytes.NewReader(raw))
	r.Host = host
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.authPublicRoutesForTest().ServeHTTP(w, r)
	return w
}
