package mcpchannel

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"piso/internal/ownfile"
)

// PinVersion is the gateway.json schema version.
const PinVersion = 1

// PinFile is the filename written into each workers/<slug>/ directory.
const PinFile = "gateway.json"

// Pin is the gateway public material workers pin against.
type Pin struct {
	Version int    `json:"version"`
	X25519  string `json:"x25519"`
	KeyID   string `json:"keyId"`
	Signing string `json:"signing"`
}

// PinFromPublic builds a pin from raw X25519 and Ed25519 public keys.
func PinFromPublic(x25519Pub, signingPub []byte) Pin {
	return Pin{
		Version: PinVersion,
		X25519:  hex.EncodeToString(x25519Pub),
		KeyID:   KeyIDHex(x25519Pub),
		Signing: hex.EncodeToString(signingPub),
	}
}

// ParsePin validates the JSON workers read from /etc/piso/gateway.json.
func ParsePin(raw []byte) (Pin, error) {
	var p Pin
	if err := json.Unmarshal(raw, &p); err != nil {
		return Pin{}, fmt.Errorf("gateway pin: %w", err)
	}
	if err := p.Validate(); err != nil {
		return Pin{}, err
	}
	return p, nil
}

// Validate checks lengths and that keyId matches the X25519 public key.
func (p Pin) Validate() error {
	if p.Version != PinVersion {
		return fmt.Errorf("gateway pin version %d", p.Version)
	}
	pub, err := hex.DecodeString(strings.ToLower(p.X25519))
	if err != nil || len(pub) != EncLen {
		return fmt.Errorf("gateway pin: invalid x25519")
	}
	if !strings.EqualFold(p.KeyID, KeyIDHex(pub)) {
		return fmt.Errorf("gateway pin: key id does not match x25519")
	}
	sig, err := hex.DecodeString(strings.ToLower(p.Signing))
	if err != nil || len(sig) != ed25519.PublicKeySize {
		return fmt.Errorf("gateway pin: invalid signing key")
	}
	return nil
}

// X25519Bytes returns the decoded encryption public key.
func (p Pin) X25519Bytes() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.ToLower(p.X25519))
}

// LoadPin reads and validates a gateway.json file.
func LoadPin(path string) (Pin, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Pin{}, err
	}
	return ParsePin(raw)
}

// WritePin atomically writes gateway.json (mode 0644: workers mount the
// directory read-only and only need the public material).
func WritePin(path string, p Pin) error {
	if err := p.Validate(); err != nil {
		return err
	}
	p.X25519 = strings.ToLower(p.X25519)
	p.KeyID = strings.ToLower(p.KeyID)
	p.Signing = strings.ToLower(p.Signing)
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return ownfile.MatchDir(path)
}
