package whitenoise

import (
	"encoding/hex"
	"errors"
	"strings"
)

const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var errPublicKey = errors.New("Enter a valid public npub; private keys and raw keys are not accepted")

func polymod(v []byte) uint32 {
	chk := uint32(1)
	gen := []uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	for _, x := range v {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(x)
		for i, g := range gen {
			if top>>i&1 != 0 {
				chk ^= g
			}
		}
	}
	return chk
}
func prefix() []byte { return []byte{3, 3, 3, 3, 0, 14, 16, 21, 2} }
func convert(data []byte, from, to uint, pad bool) ([]byte, error) {
	var acc uint32
	var bits uint
	var out []byte
	for _, v := range data {
		if uint32(v)>>from != 0 {
			return nil, errPublicKey
		}
		acc = (acc<<from | uint32(v)) & ((1 << (from + to - 1)) - 1)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&((1<<to)-1)))
		}
	}
	if pad && bits > 0 {
		out = append(out, byte(acc<<(to-bits)&((1<<to)-1)))
	} else if !pad && (bits >= from || acc<<(to-bits)&((1<<to)-1) != 0) {
		return nil, errPublicKey
	}
	return out, nil
}
func PublicKey(key string) (string, error) {
	raw, err := hex.DecodeString(key)
	if err != nil || len(raw) != 32 {
		return "", errPublicKey
	}
	data, _ := convert(raw, 8, 5, true)
	v := append(prefix(), data...)
	v = append(v, make([]byte, 6)...)
	sum := polymod(v) ^ 1
	for i := 0; i < 6; i++ {
		data = append(data, byte(sum>>uint(5*(5-i))&31))
	}
	var b strings.Builder
	b.WriteString("npub1")
	for _, x := range data {
		b.WriteByte(charset[x])
	}
	return b.String(), nil
}
func ParsePublicKey(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.ToLower(s) != s && strings.ToUpper(s) != s {
		return "", errPublicKey
	}
	s = strings.ToLower(s)
	if !strings.HasPrefix(s, "npub1") || len(s) != 63 {
		return "", errPublicKey
	}
	var data []byte
	for _, ch := range s[5:] {
		n := strings.IndexRune(charset, ch)
		if n < 0 {
			return "", errPublicKey
		}
		data = append(data, byte(n))
	}
	if polymod(append(prefix(), data...)) != 1 {
		return "", errPublicKey
	}
	raw, err := convert(data[:len(data)-6], 5, 8, false)
	if err != nil || len(raw) != 32 {
		return "", errPublicKey
	}
	return hex.EncodeToString(raw), nil
}
