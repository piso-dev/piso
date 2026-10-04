// Package mcpchannel is the shared HPKE + Ed25519 wire format for the
// encrypted worker → gateway MCP channel. Both ends import this package so
// the seal and open paths cannot drift.
package mcpchannel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/circl/hpke"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// Version is the outer framing version.
	Version byte = 0x01

	// Info is the HPKE application info string.
	Info = "piso-mcp-v1"

	// ReqLabel prefixes the fixed signature bytes.
	ReqLabel = "piso-mcp-req-v1"

	// RespExport is the HPKE exporter context for response keys.
	RespExport = "piso-mcp-resp-v1"

	KeyIDLen  = 8
	EncLen    = 32
	NonceLen  = 16
	RespNonce = 32

	// MaxRequest is the sealed request cap (8 MiB).
	MaxRequest = 8 << 20
	// MaxResponse is the sealed response cap (16 MiB).
	MaxResponse = 16 << 20

	contentType = "application/piso-mcp"
)

// ContentType is the HTTP media type for sealed bodies.
const ContentType = contentType

// Suite is DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, ChaCha20-Poly1305.
var Suite = hpke.NewSuite(hpke.KEM_X25519_HKDF_SHA256, hpke.KDF_HKDF_SHA256, hpke.AEAD_ChaCha20Poly1305)

// KeyID is the first 8 bytes of SHA-256(X25519 public key).
func KeyID(x25519Pub []byte) []byte {
	sum := sha256.Sum256(x25519Pub)
	return sum[:KeyIDLen]
}

// KeyIDHex is the hex form stored in gateway.json and request headers.
func KeyIDHex(x25519Pub []byte) string {
	return hex.EncodeToString(KeyID(x25519Pub))
}

// Inner is the HPKE plaintext of a request.
type Inner struct {
	V       int    `json:"v"`
	Pub     string `json:"pub"`
	TS      int64  `json:"ts"`
	Nonce   string `json:"nonce"`
	Content string `json:"content"`
	Sig     string `json:"sig"`
}

// SignedBytes are the fixed bytes Ed25519 signs. They bind the gateway key
// and this exact HPKE enc. Never re-serialized JSON.
func SignedBytes(gatewayX25519Pub, enc []byte, ts int64, nonce, content []byte) []byte {
	sum := sha256.Sum256(content)
	return []byte(strings.Join([]string{
		ReqLabel,
		hex.EncodeToString(gatewayX25519Pub),
		hex.EncodeToString(enc),
		strconv.FormatInt(ts, 10),
		hex.EncodeToString(nonce),
		hex.EncodeToString(sum[:]),
	}, "\n") + "\n")
}

// Request is a sealed worker call plus the fields the gateway checks.
type Request struct {
	KeyID   []byte
	Enc     []byte
	Pub     []byte
	TS      int64
	Nonce   []byte
	Content []byte
	Sig     []byte
	// Sender is the HPKE context. The gateway uses it to seal the response.
	opener hpke.Opener
}

// SealRequest encrypts content to the gateway X25519 key and signs the
// fixed bytes with the worker Ed25519 key. pub and priv are raw keys.
func SealRequest(gatewayX25519Pub, workerPub ed25519.PublicKey, workerPriv ed25519.PrivateKey, content []byte, now time.Time) ([]byte, *ResponseContext, error) {
	if len(gatewayX25519Pub) != EncLen {
		return nil, nil, fmt.Errorf("gateway public key must be %d bytes", EncLen)
	}
	if len(workerPub) != ed25519.PublicKeySize || len(workerPriv) != ed25519.PrivateKeySize {
		return nil, nil, errors.New("invalid worker signing key")
	}
	pk, err := hpke.KEM_X25519_HKDF_SHA256.Scheme().UnmarshalBinaryPublicKey(gatewayX25519Pub)
	if err != nil {
		return nil, nil, fmt.Errorf("gateway public key: %w", err)
	}
	sender, err := Suite.NewSender(pk, []byte(Info))
	if err != nil {
		return nil, nil, err
	}
	enc, sealer, err := sender.Setup(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	if len(enc) != EncLen {
		return nil, nil, fmt.Errorf("unexpected enc length %d", len(enc))
	}
	nonce := make([]byte, NonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	ts := now.Unix()
	sig := ed25519.Sign(workerPriv, SignedBytes(gatewayX25519Pub, enc, ts, nonce, content))
	inner, err := json.Marshal(Inner{
		V:       1,
		Pub:     hex.EncodeToString(workerPub),
		TS:      ts,
		Nonce:   hex.EncodeToString(nonce),
		Content: base64.StdEncoding.EncodeToString(content),
		Sig:     hex.EncodeToString(sig),
	})
	if err != nil {
		return nil, nil, err
	}
	kid := KeyID(gatewayX25519Pub)
	aad := outerAAD(kid)
	ct, err := sealer.Seal(inner, aad)
	if err != nil {
		return nil, nil, err
	}
	body := make([]byte, 0, 1+KeyIDLen+len(enc)+len(ct))
	body = append(body, Version)
	body = append(body, kid...)
	body = append(body, enc...)
	body = append(body, ct...)
	return body, &ResponseContext{enc: append([]byte(nil), enc...), sealer: sealer}, nil
}

// OpenRequest decrypts a sealed request with the gateway X25519 private key.
// It does not check freshness, replay, signature, or caller binding — the
// gateway does those in order after a successful open. The returned Request
// holds the opener used to seal the response.
func OpenRequest(gatewayX25519Priv, body []byte) (*Request, error) {
	if len(body) < 1+KeyIDLen+EncLen+16 {
		return nil, errors.New("request too short")
	}
	if body[0] != Version {
		return nil, ErrUnknownVersion
	}
	kid := body[1 : 1+KeyIDLen]
	enc := body[1+KeyIDLen : 1+KeyIDLen+EncLen]
	ct := body[1+KeyIDLen+EncLen:]
	sk, err := hpke.KEM_X25519_HKDF_SHA256.Scheme().UnmarshalBinaryPrivateKey(gatewayX25519Priv)
	if err != nil {
		return nil, fmt.Errorf("gateway private key: %w", err)
	}
	receiver, err := Suite.NewReceiver(sk, []byte(Info))
	if err != nil {
		return nil, err
	}
	opener, err := receiver.Setup(enc)
	if err != nil {
		return nil, fmt.Errorf("hpke setup: %w", err)
	}
	pt, err := opener.Open(ct, outerAAD(kid))
	if err != nil {
		return nil, fmt.Errorf("hpke open: %w", err)
	}
	var inner Inner
	if err := json.Unmarshal(pt, &inner); err != nil {
		return nil, fmt.Errorf("inner json: %w", err)
	}
	if inner.V != 1 {
		return nil, fmt.Errorf("inner version %d", inner.V)
	}
	pub, err := hex.DecodeString(inner.Pub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("inner pub")
	}
	nonce, err := hex.DecodeString(inner.Nonce)
	if err != nil || len(nonce) != NonceLen {
		return nil, errors.New("inner nonce")
	}
	content, err := base64.StdEncoding.DecodeString(inner.Content)
	if err != nil {
		return nil, errors.New("inner content")
	}
	sig, err := hex.DecodeString(inner.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("inner sig")
	}
	return &Request{
		KeyID:   append([]byte(nil), kid...),
		Enc:     append([]byte(nil), enc...),
		Pub:     pub,
		TS:      inner.TS,
		Nonce:   nonce,
		Content: content,
		Sig:     sig,
		opener:  opener,
	}, nil
}

// Verify reports whether sig is valid for this request against gatewayPub.
func (r *Request) Verify(gatewayX25519Pub []byte) bool {
	if r == nil {
		return false
	}
	msg := SignedBytes(gatewayX25519Pub, r.Enc, r.TS, r.Nonce, r.Content)
	return ed25519.Verify(r.Pub, msg, r.Sig)
}

func outerAAD(keyID []byte) []byte {
	aad := make([]byte, 1+KeyIDLen)
	aad[0] = Version
	copy(aad[1:], keyID)
	return aad
}

// ResponseContext seals or opens a response bound to one request's HPKE context.
type ResponseContext struct {
	enc    []byte
	sealer hpke.Sealer
	opener hpke.Opener
}

// SealResponse encrypts jsonrpc bytes with the exporter secret (RFC 9458 style).
// Only the gateway (holder of the opener) can produce that secret.
func (c *ResponseContext) SealResponse(jsonrpc []byte) ([]byte, error) {
	if c == nil || c.opener == nil {
		return nil, errors.New("no response context")
	}
	secret := c.opener.Export([]byte(RespExport), 32)
	return sealResponse(c.enc, secret, jsonrpc)
}

// OpenResponse decrypts a sealed response with the sender context from SealRequest.
func (c *ResponseContext) OpenResponse(body []byte) ([]byte, error) {
	if c == nil || c.sealer == nil {
		return nil, errors.New("no response context")
	}
	secret := c.sealer.Export([]byte(RespExport), 32)
	return openResponse(c.enc, secret, body)
}

// SealResponse encrypts a response with the opener from OpenRequest.
func (r *Request) SealResponse(jsonrpc []byte) ([]byte, error) {
	if r == nil || r.opener == nil {
		return nil, errors.New("no response context")
	}
	secret := r.opener.Export([]byte(RespExport), 32)
	return sealResponse(r.Enc, secret, jsonrpc)
}

// OpenSealedResponse decrypts a response with the same opener that sealed it.
// The gateway uses this to confirm a response it just produced.
func (r *Request) OpenSealedResponse(body []byte) ([]byte, error) {
	if r == nil || r.opener == nil {
		return nil, errors.New("no response context")
	}
	secret := r.opener.Export([]byte(RespExport), 32)
	return openResponse(r.Enc, secret, body)
}

func sealResponse(enc, secret, jsonrpc []byte) ([]byte, error) {
	respNonce := make([]byte, RespNonce)
	if _, err := io.ReadFull(rand.Reader, respNonce); err != nil {
		return nil, err
	}
	key, nonce, err := responseKeys(enc, respNonce, secret)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, jsonrpc, []byte{Version})
	out := make([]byte, 0, 1+RespNonce+len(ct))
	out = append(out, Version)
	out = append(out, respNonce...)
	out = append(out, ct...)
	return out, nil
}

func openResponse(enc, secret, body []byte) ([]byte, error) {
	if len(body) < 1+RespNonce+16 {
		return nil, errors.New("response too short")
	}
	if body[0] != Version {
		return nil, ErrUnknownVersion
	}
	respNonce := body[1 : 1+RespNonce]
	ct := body[1+RespNonce:]
	key, nonce, err := responseKeys(enc, respNonce, secret)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, nonce, ct, []byte{Version})
	if err != nil {
		return nil, errors.New("response open")
	}
	return pt, nil
}

func responseKeys(enc, respNonce, secret []byte) (key, nonce []byte, err error) {
	salt := make([]byte, 0, len(enc)+len(respNonce))
	salt = append(salt, enc...)
	salt = append(salt, respNonce...)
	prk := hkdf.Extract(sha256.New, secret, salt)
	key = make([]byte, 32)
	if _, err = io.ReadFull(hkdf.Expand(sha256.New, prk, []byte("key")), key); err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, 12)
	if _, err = io.ReadFull(hkdf.Expand(sha256.New, prk, []byte("nonce")), nonce); err != nil {
		return nil, nil, err
	}
	return key, nonce, nil
}

// ParseOuter reads version and key id without decrypting. Used to return
// 400 / 421 before HPKE open.
func ParseOuter(body []byte) (version byte, keyID []byte, err error) {
	if len(body) < 1+KeyIDLen {
		return 0, nil, errors.New("request too short")
	}
	return body[0], append([]byte(nil), body[1:1+KeyIDLen]...), nil
}

// Sentinel errors the gateway maps to bare statuses.
var (
	ErrUnknownVersion = errors.New("unknown version")
	ErrWrongKey       = errors.New("wrong key id")
)

// ReplayKey is the (pub, nonce) pair the gateway remembers for 2× skew.
func ReplayKey(pub, nonce []byte) string {
	return hex.EncodeToString(pub) + ":" + hex.EncodeToString(nonce)
}

// Fingerprint is the short human prefix of an Ed25519 public key hex.
func Fingerprint(pub []byte) string {
	h := hex.EncodeToString(pub)
	if len(h) < 12 {
		return h
	}
	return h[:12]
}


