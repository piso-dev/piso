package hostauth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func keyPair(t *testing.T) (privHex, pubHex string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(priv.Seed()), hex.EncodeToString(pub)
}

func signed(t *testing.T, privHex, method, path string, body []byte) *http.Request {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = mustRequest(t, method, path, nil)
	} else {
		r = mustRequest(t, method, path, body)
	}
	if err := Apply(r, privHex, time.Now()); err != nil {
		t.Fatal(err)
	}
	return r
}

func mustRequest(t *testing.T, method, path string, body []byte) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r, err := http.NewRequest(method, "http://127.0.0.1:8080"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSignedRequestVerifies(t *testing.T) {
	priv, pub := keyPair(t)
	body := []byte(`{"pubkey":"abc"}`)
	r := signed(t, priv, http.MethodPost, "/api/v1/auth/nostr-keys", body)
	if err := Verify(pub, r, r.URL.RequestURI(), time.Now()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Verification must leave the body readable for the handler behind it.
	got, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

// The signature has to bind the payload, or a request that grants authority
// ("add this pubkey") could have its body swapped while the signature stays
// valid — the path alone does not pin a key.
func TestBodyIsBoundIntoTheSignature(t *testing.T) {
	priv, pub := keyPair(t)
	r := signed(t, priv, http.MethodPost, "/api/v1/auth/nostr-keys", []byte(`{"pubkey":"mine"}`))

	other := []byte(`{"pubkey":"theirs"}`)
	r.Body = io.NopCloser(bytes.NewReader(other))
	r.ContentLength = int64(len(other))
	if err := Verify(pub, r, r.URL.RequestURI(), time.Now()); err == nil {
		t.Fatal("a swapped body must not verify")
	}
}

func TestBodylessAndEmptyBodyDiffer(t *testing.T) {
	// Both hash to something, but not the same thing — otherwise a signature
	// minted for "no body" would authorise "empty object".
	if BodyDigest(nil) == BodyDigest([]byte("{}")) {
		t.Fatal("nil and {} must have distinct digests")
	}
	priv, pub := keyPair(t)
	r := signed(t, priv, http.MethodGet, "/api/v1/auth/state", nil)
	// Replaying it with a body added must fail.
	r.Body = io.NopCloser(strings.NewReader("{}"))
	if err := Verify(pub, r, r.URL.RequestURI(), time.Now()); err == nil {
		t.Fatal("adding a body to a bodyless signature must not verify")
	}
}

func TestSignatureIsBoundToPathAndMethod(t *testing.T) {
	priv, pub := keyPair(t)
	r := signed(t, priv, http.MethodPost, "/api/v1/auth/logout-all", nil)

	sameNonce := func(method, path string) *http.Request {
		c := mustRequest(t, method, path, nil)
		for _, h := range []string{HeaderTimestamp, HeaderNonce, HeaderSignature} {
			c.Header.Set(h, r.Header.Get(h))
		}
		return c
	}
	if err := Verify(pub, sameNonce(http.MethodPost, "/api/v1/auth/nostr-keys"), "/api/v1/auth/nostr-keys", time.Now()); err == nil {
		t.Fatal("a signature must not transfer to another path")
	}
	if err := Verify(pub, sameNonce(http.MethodGet, "/api/v1/auth/logout-all"), "/api/v1/auth/logout-all", time.Now()); err == nil {
		t.Fatal("a signature must not transfer to another method")
	}
}

func TestStaleTimestampIsRefused(t *testing.T) {
	priv, pub := keyPair(t)
	r := mustRequest(t, http.MethodGet, "/api/v1/auth/state", nil)
	if err := Apply(r, priv, time.Now().Add(-MaxSkew-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, r, r.URL.RequestURI(), time.Now()); err == nil {
		t.Fatal("an old signature must expire")
	}
}

func TestMissingHeadersAreRefused(t *testing.T) {
	_, pub := keyPair(t)
	r := mustRequest(t, http.MethodGet, "/api/v1/auth/state", nil)
	if err := Verify(pub, r, "/api/v1/auth/state", time.Now()); err == nil {
		t.Fatal("want an error without headers")
	}
}
