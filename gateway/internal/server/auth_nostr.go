package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"piso/gateway/internal/store"
	"piso/internal/nostrkey"
)

// Nostr signer login.
//
// The second factor is a nostr key: the browser extension (NIP-07) or a signer
// holds the private key, the gateway only ever sees a signature over a
// challenge. It is the escape hatch passkeys structurally cannot offer — a
// passkey is bound to one origin, while this works on whatever host or IP the
// dashboard happens to be reached at, and reuses an identity the operator
// already has.
//
// The exchange follows NIP-42's shape: sign a short event whose tags carry the
// challenge and the domain. Verification is BIP340 Schnorr over secp256k1 —
// note that piso's own identity keys are Ed25519, so this is a separate
// verifier, not the same one.

const (
	// nostrKindAuth tags a sign-in event; NIP-42's kind, so existing signer UIs
	// show something recognisable.
	nostrKindAuth = 22242

	// nostrChallengeTTL bounds how long a challenge can be answered.
	nostrChallengeTTL = 5 * time.Minute

	// nostrEventSkew bounds the event's own timestamp.
	nostrEventSkew = 5 * time.Minute
)

// nostrEvent is the subset of a nostr event this gateway inspects.
type nostrEvent struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

// nostrChallenge is an outstanding sign-in attempt.
type nostrChallenge struct {
	Host    string
	Purpose string // "login" or "enroll"
	Exp     int64
}

type nostrChallenges struct {
	mu sync.Mutex
	m  map[string]nostrChallenge
}

func (c *nostrChallenges) put(challenge string, v nostrChallenge) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]nostrChallenge{}
	}
	now := time.Now().Unix()
	for k, old := range c.m {
		if now >= old.Exp {
			delete(c.m, k)
		}
	}
	c.m[challenge] = v
}

// take consumes a challenge: it can be answered exactly once.
func (c *nostrChallenges) take(challenge string) (nostrChallenge, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[challenge]
	if !ok {
		return nostrChallenge{}, false
	}
	delete(c.m, challenge)
	if time.Now().Unix() >= v.Exp {
		return nostrChallenge{}, false
	}
	return v, true
}

// The outstanding challenges live on the Server (s.nostrChallenges). In memory on
// purpose: they live for minutes and are worthless after use.

// GET /auth/nostr/challenge?purpose=login|enroll
func (s *Server) handleNostrChallenge(w http.ResponseWriter, r *http.Request) {
	purpose := r.URL.Query().Get("purpose")
	if purpose != "enroll" {
		purpose = "login"
	}
	challenge, err := randomURLToken(24)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	host := hostOnly(r.Host)
	if h := s.dashboardHost(); h != "" {
		host = h
	}
	exp := time.Now().Add(nostrChallengeTTL).Unix()
	s.nostrChallenges.put(challenge, nostrChallenge{Host: host, Purpose: purpose, Exp: exp})
	writeJSON(w, http.StatusOK, map[string]any{
		"challenge": challenge,
		"domain":    host,
		"purpose":   purpose,
		"expires":   exp,
		"kind":      nostrKindAuth,
	})
}

// POST /auth/nostr/verify -> session cookie for a registered pubkey
func (s *Server) handleNostrVerify(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
		return
	}
	ev, err := decodeNostrEvent(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	challenge, ok := challengeTag(ev)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "event carries no challenge tag"})
		return
	}
	ch, ok := s.nostrChallenges.take(challenge)
	if !ok || ch.Purpose != "login" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown or expired challenge; start again"})
		return
	}
	if err := verifyNostrEvent(ev, ch, time.Now()); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	rec, ok := s.Store.CredentialByPubkey(ev.PubKey)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":  "this nostr key is not registered with this gateway",
			"enroll": "/auth/link",
		})
		return
	}
	_ = s.Store.TouchCredential(rec.ID, time.Now())
	s.issueSession(w, rec.ID, store.CredentialNostr)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "kind": store.CredentialNostr})
}

// POST /auth/nostr/enroll -> registers a pubkey against a valid enroll link
func (s *Server) handleNostrEnroll(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
		return
	}
	var body struct {
		Event nostrEvent `json:"event"`
		Nonce string     `json:"n"`
		Exp   int64      `json:"e"`
		Sig   string     `json:"s"`
		Label string     `json:"label"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	link := decodeEnrollLink(r.URL.Query())
	if link.empty() {
		link = enrollLink{Nonce: body.Nonce, Exp: body.Exp, Sig: body.Sig}
	}
	if link.empty() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enrollment link required"})
		return
	}
	if err := s.verifyEnrollLink(link); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}
	ev := body.Event
	challenge, ok := challengeTag(ev)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "event carries no challenge tag"})
		return
	}
	ch, ok := s.nostrChallenges.take(challenge)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown or expired challenge; start again"})
		return
	}
	if err := verifyNostrEvent(ev, ch, time.Now()); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	rec, err := s.Store.AddCredential(store.CredentialRec{
		Kind:   store.CredentialNostr,
		Label:  strings.TrimSpace(body.Label),
		Pubkey: ev.PubKey,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.issueSession(w, rec.ID, store.CredentialNostr)
	writeJSON(w, http.StatusOK, map[string]any{
		"registered": true,
		"id":         rec.ID,
		"kind":       rec.Kind,
		"pubkey":     rec.Pubkey,
	})
}

// handleAddNostrKey registers a nostr public key without a browser ceremony.
//
// Enrollment through a link proves two things at once: root on this host (the
// link is signed by the identity key) and possession of the key (the browser
// signs a challenge). This endpoint deliberately proves only the first. That is
// the right trade for a fleet: an operator with root on a gateway can already
// replace the binary, read state.json and mint links, so requiring the signer to
// be present adds no security — it only adds a browser window per host, which is
// exactly the thing that leads to one shared key pasted everywhere, or to a
// gateway nobody can log in to.
//
// The request is host-signed (hostauth) and its body is bound into that
// signature, so the pubkey that lands cannot differ from the one signed for.
func (s *Server) handleAddNostrKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireHost(w, r) {
		return
	}
	var body struct {
		Pubkey string `json:"pubkey"`
		Label  string `json:"label"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	pub, err := nostrkey.Normalize(body.Pubkey)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	rec, err := s.Store.AddCredential(store.CredentialRec{
		Kind:   store.CredentialNostr,
		Label:  strings.TrimSpace(body.Label),
		Pubkey: pub,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"added":  true,
		"id":     rec.ID,
		"kind":   rec.Kind,
		"pubkey": rec.Pubkey,
		"label":  rec.Label,
	})
}

// decodeNostrEvent accepts either the bare event or {"event": ...}.
func decodeNostrEvent(r *http.Request) (nostrEvent, error) {
	var body struct {
		Event *nostrEvent `json:"event"`
		nostrEvent
	}
	if err := jsonBody(r, &body); err != nil {
		return nostrEvent{}, err
	}
	if body.Event != nil {
		return *body.Event, nil
	}
	if body.PubKey == "" && body.Sig == "" {
		return nostrEvent{}, fmt.Errorf("no nostr event in the request")
	}
	return body.nostrEvent, nil
}

func challengeTag(ev nostrEvent) (string, bool) {
	for _, tag := range ev.Tags {
		if len(tag) >= 2 && tag[0] == "challenge" {
			return tag[1], true
		}
	}
	return "", false
}

func domainTag(ev nostrEvent) (string, bool) {
	for _, tag := range ev.Tags {
		if len(tag) >= 2 && tag[0] == "domain" {
			return tag[1], true
		}
	}
	return "", false
}

// verifyNostrEvent checks everything about a sign-in event except whether the
// pubkey is allowed: kind, freshness, challenge binding, domain binding, the
// event id, and finally the Schnorr signature over that id.
func verifyNostrEvent(ev nostrEvent, ch nostrChallenge, now time.Time) error {
	if ev.Kind != nostrKindAuth {
		return fmt.Errorf("wrong event kind %d (want %d)", ev.Kind, nostrKindAuth)
	}
	if drift := now.Unix() - ev.CreatedAt; drift > int64(nostrEventSkew.Seconds()) || drift < -int64(nostrEventSkew.Seconds()) {
		return fmt.Errorf("event timestamp is outside %s of now", nostrEventSkew)
	}
	// The challenge value was already matched (and consumed) by the caller; all
	// that is left is the binding to the host the challenge was minted for.
	if ch.Host != "" {
		domain, ok := domainTag(ev)
		if !ok {
			return fmt.Errorf("event must carry a domain tag")
		}
		if !strings.EqualFold(hostOnly(domain), ch.Host) {
			return fmt.Errorf("event domain %q does not match %q", domain, ch.Host)
		}
	}
	if err := verifyNostrID(ev); err != nil {
		return err
	}
	return verifyNostrSig(ev)
}

// verifyNostrID recomputes the event id (NIP-01 canonical serialisation).
func verifyNostrID(ev nostrEvent) error {
	pub, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(ev.PubKey)))
	if err != nil || len(pub) != 32 {
		return fmt.Errorf("bad pubkey")
	}
	id, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(ev.ID)))
	if err != nil || len(id) != 32 {
		return fmt.Errorf("bad event id")
	}
	tags := ev.Tags
	if tags == nil {
		tags = [][]string{}
	}
	canonical, err := json.Marshal([]any{0, strings.ToLower(ev.PubKey), ev.CreatedAt, ev.Kind, tags, ev.Content})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	if !equalBytes(sum[:], id) {
		return fmt.Errorf("event id does not match its content")
	}
	return nil
}

// verifyNostrSig checks the BIP340 signature over the event id.
func verifyNostrSig(ev nostrEvent) error {
	pub, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(ev.PubKey)))
	if err != nil || len(pub) != 32 {
		return fmt.Errorf("bad pubkey")
	}
	sigRaw, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(ev.Sig)))
	if err != nil || len(sigRaw) != 64 {
		return fmt.Errorf("bad signature encoding")
	}
	id, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(ev.ID)))
	if err != nil || len(id) != 32 {
		return fmt.Errorf("bad event id")
	}
	pk, err := schnorr.ParsePubKey(pub)
	if err != nil {
		return fmt.Errorf("bad pubkey: %w", err)
	}
	sig, err := schnorr.ParseSignature(sigRaw)
	if err != nil {
		return fmt.Errorf("bad signature: %w", err)
	}
	if !sig.Verify(id, pk) {
		return fmt.Errorf("signature does not verify")
	}
	return nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// nostrEventID is exported for tests and for the CLI's diagnostics: it computes
// the id a signer would put in the event.
func nostrEventID(pubkey string, createdAt int64, kind int, tags [][]string, content string) (string, error) {
	if tags == nil {
		tags = [][]string{}
	}
	canonical, err := json.Marshal([]any{0, strings.ToLower(pubkey), createdAt, kind, tags, content})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
