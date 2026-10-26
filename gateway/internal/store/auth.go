package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Credential kinds. A dashboard credential proves possession of a device-bound
// key; the session it unlocks is the same regardless of kind.
const (
	CredentialPasskey = "passkey"
	CredentialNostr   = "nostr"
)

// CredentialRec is a registered dashboard credential.
//
// Passkey holds the marshalled webauthn.Credential (kept as raw JSON so this
// package does not depend on the WebAuthn library), Nostr holds the 64-hex
// x-only public key of the signer. Exactly one is set.
type CredentialRec struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Label     string          `json:"label,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
	LastUsed  time.Time       `json:"lastUsed,omitempty"`
	Passkey   json.RawMessage `json:"passkey,omitempty"`
	Pubkey    string          `json:"pubkey,omitempty"`
}

// KeyID is the stable public identifier of the credential's key material: the
// WebAuthn credential id for a passkey, the x-only pubkey for nostr. It is what
// a session is bound to, and it must not change across re-registration.
func (c CredentialRec) KeyID() string {
	if c.Kind == CredentialNostr {
		return strings.ToLower(c.Pubkey)
	}
	// A passkey's stable identifier is its WebAuthn credential id, not this
	// row's id: re-registering the same authenticator must replace the row
	// instead of leaving a second, dead copy of the same key. Fall back to the
	// row id only for a row whose key material does not decode.
	var pk struct {
		ID []byte `json:"id"`
	}
	if len(c.Passkey) > 0 && json.Unmarshal(c.Passkey, &pk) == nil && len(pk.ID) > 0 {
		return "passkey:" + hex.EncodeToString(pk.ID)
	}
	return "row:" + c.ID
}

// CredentialRecs returns a copy of the registered dashboard credentials.
func (s *Store) CredentialRecs() []CredentialRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CredentialRec, len(s.state.Credentials))
	copy(out, s.state.Credentials)
	return out
}

// CredentialByID returns one credential.
func (s *Store) CredentialByID(id string) (CredentialRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.state.Credentials {
		if c.ID == id {
			return c, true
		}
	}
	return CredentialRec{}, false
}

// CredentialByPubkey returns the nostr credential for an x-only pubkey.
func (s *Store) CredentialByPubkey(pubkey string) (CredentialRec, bool) {
	want := strings.ToLower(strings.TrimSpace(pubkey))
	if want == "" {
		return CredentialRec{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.state.Credentials {
		if c.Kind == CredentialNostr && strings.ToLower(c.Pubkey) == want {
			return c, true
		}
	}
	return CredentialRec{}, false
}

// AddCredential registers a credential. A second credential for the same key
// material replaces the first rather than duplicating it — re-running the
// enrollment flow on the same authenticator should not leave a dead row.
func (s *Store) AddCredential(rec CredentialRec) (CredentialRec, error) {
	if rec.Kind != CredentialPasskey && rec.Kind != CredentialNostr {
		return CredentialRec{}, fmt.Errorf("unknown credential kind %q", rec.Kind)
	}
	if rec.Kind == CredentialNostr && strings.TrimSpace(rec.Pubkey) == "" {
		return CredentialRec{}, fmt.Errorf("nostr credential needs a pubkey")
	}
	if rec.Kind == CredentialPasskey && len(rec.Passkey) == 0 {
		return CredentialRec{}, fmt.Errorf("passkey credential needs key material")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.ID == "" {
		id, err := newRowID()
		if err != nil {
			return CredentialRec{}, err
		}
		rec.ID = id
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	rec.Pubkey = strings.ToLower(strings.TrimSpace(rec.Pubkey))
	for i, c := range s.state.Credentials {
		if c.KeyID() != rec.KeyID() {
			continue
		}
		rec.ID = c.ID // keep the id sessions may already reference
		rec.CreatedAt = c.CreatedAt
		s.state.Credentials[i] = rec
		return rec, s.save()
	}
	s.state.Credentials = append(s.state.Credentials, rec)
	return rec, s.save()
}

// TouchCredential records a successful authentication against a credential.
func (s *Store) TouchCredential(id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.state.Credentials {
		if c.ID != id {
			continue
		}
		s.state.Credentials[i].LastUsed = at.UTC()
		return s.save()
	}
	return fmt.Errorf("credential %q not found", id)
}

// UpdatePasskey replaces a passkey's stored key material — the WebAuthn counter
// advances on every login and a stale counter would reject the next one.
func (s *Store) UpdatePasskey(id string, material json.RawMessage, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.state.Credentials {
		if c.ID != id {
			continue
		}
		if c.Kind != CredentialPasskey {
			return fmt.Errorf("credential %q is not a passkey", id)
		}
		s.state.Credentials[i].Passkey = material
		s.state.Credentials[i].LastUsed = at.UTC()
		return s.save()
	}
	return fmt.Errorf("credential %q not found", id)
}

// DeleteCredential removes one credential. Deleting the last one is allowed:
// the host keeps working through `piso gateway auth enroll`.
func (s *Store) DeleteCredential(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.state.Credentials {
		if c.ID != id {
			continue
		}
		s.state.Credentials = append(s.state.Credentials[:i], s.state.Credentials[i+1:]...)
		return s.save()
	}
	return fmt.Errorf("credential %q not found", id)
}

// SessionEpoch is the generation stamped into every dashboard session cookie.
// Bumping it invalidates all of them at once without keeping a session table.
func (s *Store) SessionEpoch() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.SessionEpoch
}

// BumpSessionEpoch invalidates every issued dashboard session.
func (s *Store) BumpSessionEpoch() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.SessionEpoch++
	return s.state.SessionEpoch, s.save()
}

func newRowID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
