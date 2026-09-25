package technocore

import (
	"bytes"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	recipient, err := Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("super-secret-api-key")
	dek, nonce, ct, err := SealPayload(plain)
	if err != nil {
		t.Fatal(err)
	}
	wrap, err := WrapDEK(recipient.EncryptionPublicKey, dek)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenEnvelope(recipient, wrap, nonce, ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("got %q", got)
	}
}

func TestEnvelopeWrongRecipient(t *testing.T) {
	a, err := Generate("https://s", "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate("https://s", "b")
	if err != nil {
		t.Fatal(err)
	}
	dek, nonce, ct, err := SealPayload([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	wrap, err := WrapDEK(a.EncryptionPublicKey, dek)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenEnvelope(b, wrap, nonce, ct); err == nil {
		t.Fatal("expected failure for other recipient")
	}
}

func TestWrapRejectsBadPubKey(t *testing.T) {
	if _, err := WrapDEK("00", make([]byte, 32)); err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenPayloadRejectsTamper(t *testing.T) {
	dek, nonce, ct, err := SealPayload([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	ct[0] ^= 0xff
	if _, err := OpenPayload(dek, nonce, ct); err == nil {
		t.Fatal("expected auth failure")
	}
}
