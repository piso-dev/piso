package nostrkey

import (
	"strings"
	"testing"
)

// The hex and npub forms of the same key: the pubkey for BIP-340 test seckey 3.
// The npub was produced by a separate BIP-173 implementation whose checksums
// were checked against the spec's own test vectors.
const (
	testHex  = "7e7e9c42a91bfef19fa929e5fda1b72e0ebc1a4c1141673e2794234d86addf4e"
	testNpub = "npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptg"
)

func TestNormalizeAcceptsBothForms(t *testing.T) {
	for _, in := range []string{testHex, strings.ToUpper(testHex), " " + testHex + "\n", testNpub} {
		got, err := Normalize(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != testHex {
			t.Fatalf("%q: want %s, got %s", in, testHex, got)
		}
	}
}

func TestNormalizeRejectsJunk(t *testing.T) {
	cases := map[string]string{
		"empty":          "",
		"short hex":      "7e7e9c42",
		"long hex":       testHex + "00",
		"not hex":        strings.Repeat("z", 64),
		"off curve":      strings.Repeat("f", 64),
		"npub checksum":  testNpub[:len(testNpub)-1] + "q",
		"npub hrp":       "naddr1" + testNpub[6:],
		"npub truncated": testNpub[:len(testNpub)-8],
		"npub padding":   testNpub,
	}
	// "npub padding" is only junk when the payload is short of a byte: corrupt a
	// middle character instead so the checksum no longer holds.
	cases["npub padding"] = testNpub[:40] + "q" + testNpub[41:]
	for name, in := range cases {
		if got, err := Normalize(in); err == nil {
			t.Fatalf("%s: want an error, got %q", name, got)
		}
	}
}

// Handing a gateway a secret key must fail loudly, not be accepted as a "public
// key" and quietly stored.
func TestNormalizeRefusesASecretKey(t *testing.T) {
	// nsec1 form of an all-zero secret: enough to prove the branch, and the
	// checksum is real, so this is only rejected by the nsec rule.
	nsec := "nsec1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqwkhnav"
	_, err := Normalize(nsec)
	if err == nil {
		t.Fatal("an nsec must be refused")
	}
	if !strings.Contains(err.Error(), "secret key") {
		t.Fatalf("the error should say what went wrong, got: %v", err)
	}
}

func TestNormalizeIsStable(t *testing.T) {
	// Idempotent: the gateway stores Normalize's output and later compares it to
	// whatever the CLI sends, so a second pass must be a no-op.
	once, err := Normalize(testNpub)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Normalize(once)
	if err != nil {
		t.Fatal(err)
	}
	if once != twice {
		t.Fatalf("not idempotent: %s vs %s", once, twice)
	}
}
