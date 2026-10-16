package technocore

import (
	"net/http"
	"testing"
	"time"
)

func TestSignAndVerifyAuth(t *testing.T) {
	id, err := Generate("https://s", "n")
	if err != nil {
		t.Fatal(err)
	}
	ts := "1700000000"
	nonce, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	hash := BodyHashHex(nil)
	sig, err := SignAuth(id, ts, "GET", "/api/v1/sync/secrets", hash, nonce)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	if err := VerifyAuth(id.SigningPublicKey, ts, "GET", "/api/v1/sync/secrets", hash, nonce, sig, now); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAuth(id.SigningPublicKey, ts, "POST", "/api/v1/sync/secrets", hash, nonce, sig, now); err == nil {
		t.Fatal("method must be bound")
	}
	if err := VerifyAuth(id.SigningPublicKey, ts, "GET", "/other", hash, nonce, sig, now); err == nil {
		t.Fatal("path must be bound")
	}
	if err := VerifyAuth(id.SigningPublicKey, ts, "GET", "/api/v1/sync/secrets", BodyHashHex([]byte("x")), nonce, sig, now); err == nil {
		t.Fatal("body hash must be bound")
	}
	if err := VerifyAuth(id.SigningPublicKey, ts, "GET", "/api/v1/sync/secrets", hash, nonce, sig, now.Add(2*time.Minute)); err == nil {
		t.Fatal("skew must be rejected")
	}
}

func TestSignRequestSetsHeaders(t *testing.T) {
	id, err := Generate("https://s", "n")
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://s/api/v1/sync/secrets", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := SignRequest(req, id, nil); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get(HeaderPubkey) != id.SigningPublicKey {
		t.Fatal("pubkey header")
	}
	if req.Header.Get(HeaderNonce) == "" || req.Header.Get(HeaderSignature) == "" {
		t.Fatal("missing nonce or signature")
	}
}

func TestCanonicalAuthMessageStable(t *testing.T) {
	a := CanonicalAuthMessage("1", "AABB", "get", "/p", "00", "ff")
	b := CanonicalAuthMessage("1", "aabb", "GET", "/p", "00", "FF")
	if string(a) != string(b) {
		t.Fatalf("canonicalization\n%s\n%s", a, b)
	}
}
