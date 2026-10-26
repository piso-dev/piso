package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"piso/gateway/internal/store"
)

// Passkey (WebAuthn) is the primary dashboard credential: one tap, nothing to
// type, and the private key never leaves the authenticator.
//
// The operator is a single account, so the relying party uses one fixed user
// handle. Registering a second authenticator (a phone next to the laptop, a
// backup key) adds a credential to the same user rather than a new account.

// dashUser implements webauthn.User for the one dashboard operator.
type dashUser struct {
	creds []webauthn.Credential
}

// dashUserHandle is stable across enrollments and never shown to anyone. Using
// a literal keeps it stable even if the identity file is replaced.
var dashUserHandle = []byte("piso-gateway-operator-handle-v1")

func (d dashUser) WebAuthnID() []byte                         { return dashUserHandle }
func (d dashUser) WebAuthnName() string                       { return "operator" }
func (d dashUser) WebAuthnDisplayName() string                { return "piso gateway operator" }
func (d dashUser) WebAuthnCredentials() []webauthn.Credential { return d.creds }

// webauthnUser assembles the operator with its currently registered passkeys.
func (s *Server) webauthnUser() (dashUser, error) {
	var creds []webauthn.Credential
	for _, rec := range s.Store.CredentialRecs() {
		if rec.Kind != store.CredentialPasskey || len(rec.Passkey) == 0 {
			continue
		}
		var c webauthn.Credential
		if err := json.Unmarshal(rec.Passkey, &c); err != nil {
			continue // a corrupt row must not lock the others out
		}
		creds = append(creds, c)
	}
	return dashUser{creds: creds}, nil
}

// passkeyRelyingParty builds the WebAuthn config for this request.
//
// The RP id is the dashboard's hostname (no scheme, no port) and the allowed
// origins are exactly the origin the browser is on. Pinning it from
// PISO_DASHBOARD_HOST when set keeps it stable even if a proxy rewrites Host.
func (s *Server) passkeyRelyingParty(r *http.Request) (*webauthn.WebAuthn, error) {
	host := s.dashboardHost()
	if host == "" {
		host = hostOnly(r.Host)
	}
	if host == "" {
		return nil, fmt.Errorf("cannot determine the dashboard hostname")
	}
	if isIPLiteral(host) {
		return nil, fmt.Errorf("passkeys need the dashboard's DNS name, not an IP address")
	}
	scheme := "https"
	if p := r.Header.Get("X-Forwarded-Proto"); p == "http" {
		scheme = "http"
	} else if r.TLS == nil && isLoopbackHost(host) {
		scheme = "http"
	}
	return webauthn.New(&webauthn.Config{
		RPID:          host,
		RPDisplayName: "piso gateway",
		RPOrigins:     []string{scheme + "://" + host},
	})
}

func isIPLiteral(host string) bool {
	if host == "" {
		return false
	}
	// A dotted-quad or a colon means an address, never a registrable name.
	if strings.Contains(host, ":") {
		return true
	}
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		for _, ch := range p {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || strings.HasPrefix(host, "127.") || host == "::1"
}

// jsonBody decodes a small JSON body, tolerating an empty one.
func jsonBody(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16))
	if err := dec.Decode(dst); err != nil {
		if strings.Contains(err.Error(), "EOF") {
			return nil
		}
		return err
	}
	return nil
}

// storePasskey persists a freshly registered (or counter-updated) credential.
func (s *Server) storePasskey(cred *webauthn.Credential, label string, replaceID string) (store.CredentialRec, error) {
	raw, err := json.Marshal(cred)
	if err != nil {
		return store.CredentialRec{}, err
	}
	if replaceID != "" {
		if err := s.Store.UpdatePasskey(replaceID, raw, time.Now()); err != nil {
			return store.CredentialRec{}, err
		}
		rec, ok := s.Store.CredentialByID(replaceID)
		if !ok {
			return store.CredentialRec{}, fmt.Errorf("credential %q disappeared", replaceID)
		}
		return rec, nil
	}
	return s.Store.AddCredential(store.CredentialRec{
		Kind:    store.CredentialPasskey,
		Label:   label,
		Passkey: raw,
	})
}

// --- login ------------------------------------------------------------------

// POST /auth/passkey/login/begin -> assertion options
func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
		return
	}
	rp, err := s.passkeyRelyingParty(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	user, err := s.webauthnUser()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(user.creds) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":  "no passkey is registered yet",
			"enroll": "/auth/link",
		})
		return
	}
	assertion, session, err := rp.BeginLogin(user)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	data, _ := json.Marshal(session)
	if err := s.setCeremony(w, ceremony{Purpose: "passkey-login", Data: data}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, assertion)
}

// POST /auth/passkey/login/finish -> session cookie
func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
		return
	}
	rp, err := s.passkeyRelyingParty(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ce, ok := s.takeCeremony(w, r, "passkey-login")
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no ceremony in progress; start again"})
		return
	}
	var session webauthn.SessionData
	if err := json.Unmarshal(ce.Data, &session); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "corrupt ceremony"})
		return
	}
	user, err := s.webauthnUser()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cred, err := rp.FinishLogin(user, session, r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "passkey rejected: " + err.Error()})
		return
	}
	// FinishLogin advances the authenticator counter; persist it or the next
	// login looks like a cloned authenticator and is refused.
	rec, err := s.findPasskey(cred.ID)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	if _, err := s.storePasskey(cred, rec.Label, rec.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.issueSession(w, rec.ID, store.CredentialPasskey)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "kind": store.CredentialPasskey})
}

// findPasskey locates the stored row for a WebAuthn credential id.
func (s *Server) findPasskey(id []byte) (store.CredentialRec, error) {
	for _, rec := range s.Store.CredentialRecs() {
		if rec.Kind != store.CredentialPasskey || len(rec.Passkey) == 0 {
			continue
		}
		var c webauthn.Credential
		if err := json.Unmarshal(rec.Passkey, &c); err != nil {
			continue
		}
		if string(c.ID) == string(id) {
			return rec, nil
		}
	}
	return store.CredentialRec{}, fmt.Errorf("this passkey is not registered with this gateway")
}

// --- registration -----------------------------------------------------------

// enrollFromRequest reads and verifies the enrollment link from the query or
// the JSON body. Re-verified on the finish half, because the ceremony cookie
// alone must not be able to add a credential.
func enrollFromRequest(r *http.Request, body *enrollBody) (enrollLink, error) {
	link := decodeEnrollLink(r.URL.Query())
	if link.empty() && body != nil {
		link = enrollLink{Nonce: body.Nonce, Exp: body.Exp, Sig: body.Sig}
	}
	if link.empty() {
		return link, fmt.Errorf("enrollment link required")
	}
	return link, nil
}

type enrollBody struct {
	Nonce string `json:"n"`
	Exp   int64  `json:"e"`
	Sig   string `json:"s"`
	Label string `json:"label,omitempty"`
}

// POST /auth/passkey/register/begin -> creation options
func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
		return
	}
	var body enrollBody
	if err := jsonBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	link, err := enrollFromRequest(r, &body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.verifyEnrollLink(link); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	rp, err := s.passkeyRelyingParty(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	user, err := s.webauthnUser()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	opts := []webauthn.RegistrationOption{
		// Resident keys make the passkey discoverable (no username to type).
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred),
	}
	if len(user.creds) > 0 {
		opts = append(opts, webauthn.WithExclusions(webauthn.Credentials(user.creds).CredentialDescriptors()))
	}
	creation, session, err := rp.BeginRegistration(user, opts...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	data, _ := json.Marshal(session)
	if err := s.setCeremony(w, ceremony{Purpose: "passkey-register", Link: link, Data: data}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, creation)
}

// POST /auth/passkey/register/finish -> credential stored + session cookie
func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
		return
	}
	// The request body is the attestation, so the link is read from the sealed
	// ceremony cookie written by the begin half.
	ce, ok := s.takeCeremony(w, r, "passkey-register")
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no registration in progress; start again"})
		return
	}
	// Second verification: registration must be authorised by the link, not by
	// the cookie alone. The nonce was already consumed, so re-verify the
	// signature and expiry without consuming again.
	id, err := s.identity()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := verifyLinkSignature(id.SigningPublicKey, ce.Link); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	rp, err := s.passkeyRelyingParty(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var session webauthn.SessionData
	if err := json.Unmarshal(ce.Data, &session); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "corrupt ceremony"})
		return
	}
	user, err := s.webauthnUser()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cred, err := rp.FinishRegistration(user, session, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "registration rejected: " + err.Error()})
		return
	}
	rec, err := s.storePasskey(cred, labelFor(r, ""), "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.issueSession(w, rec.ID, store.CredentialPasskey)
	writeJSON(w, http.StatusOK, map[string]any{
		"registered": true,
		"id":         rec.ID,
		"kind":       rec.Kind,
		"label":      rec.Label,
	})
}

// labelFor turns a UA string into a short default label, so `piso gateway auth
// status` says which device a credential belongs to.
func labelFor(r *http.Request, given string) string {
	if strings.TrimSpace(given) != "" {
		return strings.TrimSpace(given)
	}
	ua := r.Header.Get("User-Agent")
	switch {
	case strings.Contains(ua, "iPhone"):
		return "iPhone"
	case strings.Contains(ua, "iPad"):
		return "iPad"
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "Macintosh"):
		return "Mac"
	case strings.Contains(ua, "Windows"):
		return "Windows"
	case strings.Contains(ua, "Linux"):
		return "Linux"
	}
	return "passkey"
}

// --- enrollment page --------------------------------------------------------

// handleEnrollPage serves the enrollment UI, but only for a link that verifies.
// An unauthenticated stranger gets a 403, not a button that adds a passkey.
func (s *Server) handleEnrollPage(w http.ResponseWriter, r *http.Request) {
	link := decodeEnrollLink(r.URL.Query())
	if link.empty() {
		s.serveAuthPage(w, r, "denied")
		return
	}
	if err := s.verifyEnrollLink(link); err != nil {
		s.serveAuthPage(w, r, "denied")
		return
	}
	s.serveAuthPage(w, r, "enroll")
}

// verifyLinkSignature checks the link without consuming its nonce. Used on the
// finish half of a ceremony, where the nonce was consumed by the begin half.
func verifyLinkSignature(pubHex string, link enrollLink) error {
	if link.empty() {
		return fmt.Errorf("enrollment link required")
	}
	return verifyLinkAt(pubHex, link, time.Now())
}
