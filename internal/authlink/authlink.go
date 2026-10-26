// Package authlink mints and verifies the one-time dashboard enrollment link.
//
// The gateway dashboard has no password: the first credential (a passkey or a
// nostr signer) is registered through a short-lived link that `piso gateway auth
// enroll` prints on the host. Root on the host is the authentication event, and
// the proof is the gateway's own Ed25519 identity key — the same key the
// coordinator knows it by. Nothing secret is ever printed into `docker logs`.
package authlink

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Version prefixes the signed message so a signature can never be replayed
// against a different purpose that happens to share the key.
const Version = "piso-dashboard-enroll.v1"

// TTL is how long a minted link stays valid.
const TTL = 10 * time.Minute

// MaxLifetime bounds a link's expiry regardless of what the signing side chose,
// so a hand-made link cannot be long-lived.
const MaxLifetime = TTL + time.Minute

// ErrInvalid is returned for a link that does not verify.
var ErrInvalid = errors.New("invalid enrollment link")

// Message is the canonical byte string the CLI signs and the gateway verifies.
// The nonce makes every link single-use; the expiry bounds it in time.
func Message(nonce string, exp int64) string {
	return strings.Join([]string{Version, nonce, strconv.FormatInt(exp, 10)}, "\n")
}

// NewNonce returns a fresh 128-bit nonce, hex encoded.
func NewNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Sign returns the hex signature of Message(nonce, exp) under privHex — the
// identity's hex Ed25519 seed, exactly as stored in identity.json.
func Sign(privHex, nonce string, exp int64) (string, error) {
	key, err := privateKey(privHex)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(ed25519.Sign(key, []byte(Message(nonce, exp)))), nil
}

// Verify checks a link against the gateway's hex public key and the clock.
func Verify(pubHex, nonce, sigHex string, exp int64, now time.Time) error {
	if nonce == "" || sigHex == "" {
		return fmt.Errorf("%w: nonce and signature are required", ErrInvalid)
	}
	if exp <= now.Unix() {
		return fmt.Errorf("%w: link expired", ErrInvalid)
	}
	if exp > now.Add(MaxLifetime).Unix() {
		return fmt.Errorf("%w: link lifetime is longer than %s", ErrInvalid, MaxLifetime)
	}
	pub, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: bad public key", ErrInvalid)
	}
	sig, err := hex.DecodeString(strings.TrimSpace(sigHex))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: bad signature encoding", ErrInvalid)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(Message(nonce, exp)), sig) {
		return fmt.Errorf("%w: signature does not match this gateway", ErrInvalid)
	}
	return nil
}

// Query renders just the link parameters, for callers that need the raw form.
func Query(nonce, sigHex string, exp int64) string {
	return "?n=" + nonce + "&e=" + strconv.FormatInt(exp, 10) + "&s=" + sigHex
}

// URL renders the enroll link for a dashboard base (scheme and host, no path).
func URL(base, nonce, sigHex string, exp int64) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	q := "n=" + nonce + "&e=" + strconv.FormatInt(exp, 10) + "&s=" + sigHex
	if strings.Contains(base, "?") {
		return base + "&" + q
	}
	return base + "/auth/link?" + q
}

func privateKey(privHex string) (ed25519.PrivateKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(privHex))
	if err != nil {
		return nil, fmt.Errorf("identity key: %w", err)
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, fmt.Errorf("identity key: unexpected length %d", len(raw))
	}
}
