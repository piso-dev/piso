// Package nostrkey parses the two ways an operator names a nostr identity.
//
// A nostr public key is 32 x-only bytes. People hold it as 64 hex characters
// (what a NIP-07 extension shows as "hex") or as a bech32 npub (what almost
// every social client shows). Accepting both matters here because the caller is
// a human copying a value off their signer: asking them to convert it by hand
// is how a gateway ends up with a credential nobody holds.
//
// Normalize also checks the point is really on secp256k1. A 64-character typo
// is far more likely than a deliberate malformed key, and a credential that
// cannot be used is a gateway nobody can sign in to. Failing at enrollment is
// the only cheap moment to fail.
package nostrkey

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// Size is the length of an x-only public key in bytes.
const Size = 32

// Normalize returns the lowercase hex form of a hex or npub public key.
func Normalize(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("nostr key: empty")
	}
	raw, err := decode(s)
	if err != nil {
		return "", err
	}
	if _, err := schnorr.ParsePubKey(raw); err != nil {
		return "", fmt.Errorf("nostr key: not a valid secp256k1 public key")
	}
	return hex.EncodeToString(raw), nil
}

func decode(s string) ([]byte, error) {
	if strings.Contains(s, "1") && !isHex(s) {
		return decodeBech32(s)
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("nostr key: not hex or npub: %w", err)
	}
	if len(raw) != Size {
		return nil, fmt.Errorf("nostr key: want %d bytes, got %d", Size, len(raw))
	}
	return raw, nil
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// bech32 decoding, as specified by BIP-173. npub uses bech32 (checksum constant
// 1), not bech32m, so the two can be told apart: bech32m keys are rejected
// rather than silently accepted.
const (
	charset    = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	hrpNpub    = "npub"
	bech32Want = 1
)

var generator = [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

func decodeBech32(s string) ([]byte, error) {
	if s != strings.ToLower(s) && s != strings.ToUpper(s) {
		return nil, fmt.Errorf("nostr key: mixed-case npub")
	}
	s = strings.ToLower(s)
	pos := strings.LastIndexByte(s, '1')
	if pos < 1 || pos+7 > len(s) {
		return nil, fmt.Errorf("nostr key: malformed npub")
	}
	hrp := s[:pos]
	if hrp != hrpNpub && hrp != "nsec" {
		return nil, fmt.Errorf("nostr key: not an npub")
	}
	if hrp == "nsec" {
		// Refuse rather than decode: an nsec is a secret key, and whatever the
		// caller meant to do, they do not mean to hand their secret to a gateway.
		return nil, fmt.Errorf("nostr key: that is a secret key (nsec), not a public key")
	}
	data := make([]byte, 0, len(s)-pos-1)
	for _, c := range []byte(s[pos+1:]) {
		i := strings.IndexByte(charset, c)
		if i < 0 {
			return nil, fmt.Errorf("nostr key: invalid character %q in npub", c)
		}
		data = append(data, byte(i))
	}
	if polymod(append(hrpExpand(hrp), data...)) != bech32Want {
		return nil, fmt.Errorf("nostr key: bad npub checksum")
	}
	payload, err := fromFiveBits(data[:len(data)-6])
	if err != nil {
		return nil, err
	}
	if len(payload) != Size {
		return nil, fmt.Errorf("nostr key: want %d bytes, got %d", Size, len(payload))
	}
	return payload, nil
}

// fromFiveBits converts 5-bit groups to bytes. Trailing bits must be padding:
// fewer than 5 of them, and all zero.
func fromFiveBits(data []byte) ([]byte, error) {
	var (
		acc  uint32
		bits uint
		out  []byte
	)
	for _, v := range data {
		if v > 31 {
			return nil, fmt.Errorf("nostr key: bad npub data")
		}
		acc = acc<<5 | uint32(v)
		bits += 5
		for bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>bits))
		}
	}
	if bits >= 5 || acc&(1<<bits-1) != 0 {
		return nil, fmt.Errorf("nostr key: bad npub padding")
	}
	return out, nil
}

func hrpExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

func polymod(values []byte) uint32 {
	var chk uint32 = 1
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= generator[i]
			}
		}
	}
	return chk
}
