package technocore

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	EnvelopeVersion byte = 0x01
	EnvelopeAlg          = "chacha20poly1305-v1"
	wrapInfo             = "piso-envelope-wrap-v1"
	payloadAAD           = "piso-secret-v1"
	x25519Size           = 32
)

// Wrap blob: version (1) || ephemeral_x25519_pub (32) || nonce (12) || sealed_dek (48)

// SealPayload encrypts plaintext with a random 32-byte DEK (ChaCha20-Poly1305).
func SealPayload(plaintext []byte) (dek, nonce, ciphertext []byte, err error) {
	dek = make([]byte, chacha20poly1305.KeySize)
	if _, err = io.ReadFull(rand.Reader, dek); err != nil {
		return nil, nil, nil, err
	}
	aead, err := chacha20poly1305.New(dek)
	if err != nil {
		return nil, nil, nil, err
	}
	nonce = make([]byte, chacha20poly1305.NonceSize)
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, nil, err
	}
	ciphertext = aead.Seal(nil, nonce, plaintext, []byte(payloadAAD))
	return dek, nonce, ciphertext, nil
}

// OpenPayload decrypts a payload sealed by SealPayload.
func OpenPayload(dek, nonce, ciphertext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(dek)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ciphertext, []byte(payloadAAD))
}

// WrapDEK encrypts dek to recipient's X25519 public key.
func WrapDEK(recipientPubHex string, dek []byte) ([]byte, error) {
	if len(dek) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("dek must be %d bytes", chacha20poly1305.KeySize)
	}
	pubRaw, err := hex.DecodeString(recipientPubHex)
	if err != nil || len(pubRaw) != x25519Size {
		return nil, fmt.Errorf("invalid recipient encryption public key")
	}
	recipient, err := ecdh.X25519().NewPublicKey(pubRaw)
	if err != nil {
		return nil, fmt.Errorf("invalid recipient encryption public key: %w", err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(recipient)
	if err != nil {
		return nil, err
	}
	wrapKey, err := deriveWrapKey(shared)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(wrapKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, chacha20poly1305.NonceSize)
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := aead.Seal(nil, nonce, dek, []byte(wrapInfo))
	out := make([]byte, 0, 1+x25519Size+len(nonce)+len(sealed))
	out = append(out, EnvelopeVersion)
	out = append(out, eph.PublicKey().Bytes()...)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// UnwrapDEK opens a blob produced by WrapDEK.
func UnwrapDEK(recipientPriv *ecdh.PrivateKey, blob []byte) ([]byte, error) {
	want := 1 + x25519Size + chacha20poly1305.NonceSize + chacha20poly1305.KeySize + chacha20poly1305.Overhead
	if len(blob) != want {
		return nil, fmt.Errorf("wrap blob length %d, want %d", len(blob), want)
	}
	if blob[0] != EnvelopeVersion {
		return nil, fmt.Errorf("unsupported wrap version %d", blob[0])
	}
	off := 1
	ephPub, err := ecdh.X25519().NewPublicKey(blob[off : off+x25519Size])
	if err != nil {
		return nil, fmt.Errorf("ephemeral public key: %w", err)
	}
	off += x25519Size
	nonce := blob[off : off+chacha20poly1305.NonceSize]
	off += chacha20poly1305.NonceSize
	sealed := blob[off:]
	shared, err := recipientPriv.ECDH(ephPub)
	if err != nil {
		return nil, err
	}
	wrapKey, err := deriveWrapKey(shared)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(wrapKey)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, sealed, []byte(wrapInfo))
}

func deriveWrapKey(shared []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, shared, nil, wrapInfo, chacha20poly1305.KeySize)
}

// OpenEnvelope is the gateway-side helper: unwrap DEK then decrypt payload.
func OpenEnvelope(id Identity, wrapBlob, nonce, ciphertext []byte) ([]byte, error) {
	priv, err := id.EncryptionPrivate()
	if err != nil {
		return nil, err
	}
	dek, err := UnwrapDEK(priv, wrapBlob)
	if err != nil {
		return nil, err
	}
	return OpenPayload(dek, nonce, ciphertext)
}
