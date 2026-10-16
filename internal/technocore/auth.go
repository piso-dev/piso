package technocore

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	AuthScheme      = "piso-gateway-auth"
	AuthVersion     = "v1"
	AuthMaxSkew     = 60 * time.Second
	NonceBytes      = 16
	HeaderPubkey    = "X-Piso-Pubkey"
	HeaderTimestamp = "X-Piso-Timestamp"
	HeaderNonce     = "X-Piso-Nonce"
	HeaderSignature = "X-Piso-Signature"
	HeaderPollToken = "X-Piso-Poll-Token"
	WSMethod        = "WS"
	WSPath          = "/ws/gateway/"
)

// CanonicalAuthMessage is the exact byte string Ed25519-signed for HTTP and WS.
// Fields are newline-separated; a trailing newline is included.
func CanonicalAuthMessage(timestamp, pubkey, method, path, bodyHash, nonce string) []byte {
	return []byte(strings.Join([]string{
		AuthScheme,
		AuthVersion,
		timestamp,
		strings.ToLower(strings.TrimSpace(pubkey)),
		strings.ToUpper(strings.TrimSpace(method)),
		path,
		strings.ToLower(strings.TrimSpace(bodyHash)),
		strings.ToLower(strings.TrimSpace(nonce)),
	}, "\n") + "\n")
}

// BodyHashHex is SHA-256 of the request body (empty body is the hash of zero bytes).
func BodyHashHex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// NewNonce returns 16 random bytes as lowercase hex.
func NewNonce() (string, error) {
	b := make([]byte, NonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SignAuth signs a canonical auth message with the identity's signing key.
func SignAuth(id Identity, timestamp, method, path, bodyHash, nonce string) (string, error) {
	priv, err := id.SigningPrivate()
	if err != nil {
		return "", err
	}
	msg := CanonicalAuthMessage(timestamp, id.SigningPublicKey, method, path, bodyHash, nonce)
	return hex.EncodeToString(ed25519.Sign(priv, msg)), nil
}

// VerifyAuth checks an Ed25519 signature over the canonical message.
func VerifyAuth(pubkeyHex, timestamp, method, path, bodyHash, nonce, signatureHex string, now time.Time) error {
	pub, err := hex.DecodeString(strings.TrimSpace(pubkeyHex))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid pubkey")
	}
	sig, err := hex.DecodeString(strings.TrimSpace(signatureHex))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid signature")
	}
	nonceRaw, err := hex.DecodeString(strings.TrimSpace(nonce))
	if err != nil || len(nonceRaw) != NonceBytes {
		return fmt.Errorf("invalid nonce")
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp")
	}
	t := time.Unix(ts, 0)
	skew := now.Sub(t)
	if skew < 0 {
		skew = -skew
	}
	if skew > AuthMaxSkew {
		return fmt.Errorf("timestamp outside allowed window")
	}
	msg := CanonicalAuthMessage(timestamp, pubkeyHex, method, path, bodyHash, nonce)
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return fmt.Errorf("bad signature")
	}
	return nil
}

// SignRequest sets gateway auth headers on an HTTP request.
func SignRequest(req *http.Request, id Identity, body []byte) error {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce, err := NewNonce()
	if err != nil {
		return err
	}
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	hash := BodyHashHex(body)
	sig, err := SignAuth(id, ts, req.Method, path, hash, nonce)
	if err != nil {
		return err
	}
	req.Header.Set(HeaderPubkey, strings.ToLower(id.SigningPublicKey))
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, sig)
	return nil
}
