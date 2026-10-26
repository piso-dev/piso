// Package hostauth signs requests from the piso CLI running on the gateway host.
//
// The dashboard's control API is gated once the gateway is reachable off-host,
// but the CLI is not a remote caller: it runs on the box, as root, with the
// gateway's identity key in hand. Rather than open a second unauthenticated
// door (a loopback exemption does not work through Docker's port publishing —
// the container sees the bridge address, not 127.0.0.1), the CLI proves
// possession of the identity signing key on every administrative call.
//
// This mirrors the scheme the coordinator already uses for gateway requests
// (server/app/core/gateway_auth.py), so there is one idea to understand.
package hostauth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Version prefixes the signed message, so a host signature can never be
// replayed as an enrollment link (both are signed with the identity key).
//
// v2 binds the request body into the signature. Without it a request that
// carries authority in its payload — "add this nostr pubkey as an operator" —
// could have that payload swapped in flight by anyone who can see the request,
// leaving the signature valid. The path alone does not pin a pubkey.
const Version = "piso-host-auth.v2"

// MaxSkew is how far the request clock may differ from the gateway's.
const MaxSkew = 60 * time.Second

// Header names carried on an administrative request.
const (
	HeaderTimestamp = "X-Piso-Host-Ts"
	HeaderNonce     = "X-Piso-Host-Nonce"
	HeaderSignature = "X-Piso-Host-Sig"
)

// ErrInvalid is returned for a request that does not verify.
var ErrInvalid = errors.New("invalid host authentication")

// maxBody is the largest body a signed request may carry. The CLI sends small
// JSON documents; anything bigger is not one of ours.
const maxBody = 1 << 20

// Message is the canonical string signed by the CLI: method and path bind the
// signature to this one call, the body digest binds the payload, the timestamp
// bounds it in time, and the nonce makes it single-use.
func Message(method, path string, ts int64, nonce, bodyDigest string) string {
	return strings.Join([]string{
		Version,
		strings.ToUpper(strings.TrimSpace(method)),
		path,
		bodyDigest,
		strconv.FormatInt(ts, 10),
		nonce,
	}, "\n")
}

// BodyDigest is the hash of the request body that goes into the signature. An
// empty body has a digest too, so a bodyless request and a request with an empty
// object are distinct.
func BodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Sign returns the hex signature for one host-authenticated request.
func Sign(privHex, method, path string, ts int64, nonce, bodyDigest string) (string, error) {
	key, err := privateKey(privHex)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(ed25519.Sign(key, []byte(Message(method, path, ts, nonce, bodyDigest)))), nil
}

// Apply signs an outgoing request and adds the three headers. It reads and
// replaces the body, so the caller may still send it.
func Apply(req *http.Request, privHex string, now time.Time) error {
	body, err := drainBody(req)
	if err != nil {
		return err
	}
	nonce, err := nonce()
	if err != nil {
		return err
	}
	ts := now.Unix()
	sig, err := Sign(privHex, req.Method, req.URL.RequestURI(), ts, nonce, BodyDigest(body))
	if err != nil {
		return err
	}
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, sig)
	return nil
}

// Verify checks the three headers against pubHex. path must be the exact
// request URI the client signed (path plus query).
func Verify(pubHex string, r *http.Request, path string, now time.Time) error {
	tsRaw := strings.TrimSpace(r.Header.Get(HeaderTimestamp))
	nonce := strings.TrimSpace(r.Header.Get(HeaderNonce))
	sigHex := strings.TrimSpace(r.Header.Get(HeaderSignature))
	if tsRaw == "" || nonce == "" || sigHex == "" {
		return fmt.Errorf("%w: missing headers", ErrInvalid)
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad timestamp", ErrInvalid)
	}
	if skew := now.Sub(time.Unix(ts, 0)); skew > MaxSkew || skew < -MaxSkew {
		return fmt.Errorf("%w: timestamp outside %s", ErrInvalid, MaxSkew)
	}
	pub, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: bad public key", ErrInvalid)
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: bad signature encoding", ErrInvalid)
	}
	body, err := drainBody(r)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	msg := Message(r.Method, path, ts, nonce, BodyDigest(body))
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(msg), sig) {
		return fmt.Errorf("%w: signature mismatch", ErrInvalid)
	}
	if nonce == "" {
		return fmt.Errorf("%w: empty nonce", ErrInvalid)
	}
	return nil
}

// drainBody returns the request body and puts it back for the handler. Verify
// runs before the handler, so a request body must survive the check.
func drainBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	_ = r.Body.Close()
	if len(b) > maxBody {
		return nil, fmt.Errorf("body too large")
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, nil
}

// NonceKey is the replay-cache key for a verified request.
func NonceKey(nonce string) string { return "hostauth:" + nonce }

func nonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
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
