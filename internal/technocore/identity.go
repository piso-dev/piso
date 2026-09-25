// Package technocore is the shared enrollment identity, request authentication,
// and secret-envelope crypto used by the piso CLI and the gateway process.
package technocore

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	StatusNew     = "new"
	StatusPending = "pending"
	StatusActive  = "active"

	identityFileVersion = 1
)

// Identity is the gateway (or future worker) device identity stored on disk.
// Signing and encryption keys are distinct. Private keys never leave this file.
type Identity struct {
	Version               int    `json:"version"`
	ServerURL             string `json:"serverUrl"`
	Name                  string `json:"name"`
	SigningPublicKey      string `json:"signingPublicKey"`
	SigningPrivateKey     string `json:"signingPrivateKey"`
	EncryptionPublicKey   string `json:"encryptionPublicKey"`
	EncryptionPrivateKey  string `json:"encryptionPrivateKey"`
	PairingID             string `json:"pairingId,omitempty"`
	PollToken             string `json:"pollToken,omitempty"`
	PrincipalID           string `json:"principalId,omitempty"`
	Status                string `json:"status"`
	CreatedAt             string `json:"createdAt,omitempty"`
}

// Path returns $dataDir/technocore/identity.json.
func Path(dataDir string) string {
	return filepath.Join(dataDir, "technocore", "identity.json")
}

// Fingerprint is a short, human-compared prefix of the signing public key.
func (id Identity) Fingerprint() string {
	pub := strings.ToLower(id.SigningPublicKey)
	if len(pub) < 12 {
		return pub
	}
	return pub[:12]
}

// Generate creates a new identity with fresh signing and encryption keys.
func Generate(serverURL, name string) (Identity, error) {
	signPub, signPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("signing key: %w", err)
	}
	encPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("encryption key: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return Identity{
		Version:              identityFileVersion,
		ServerURL:            strings.TrimRight(serverURL, "/"),
		Name:                 name,
		SigningPublicKey:     hex.EncodeToString(signPub),
		SigningPrivateKey:    hex.EncodeToString(signPriv.Seed()),
		EncryptionPublicKey:  hex.EncodeToString(encPriv.PublicKey().Bytes()),
		EncryptionPrivateKey: hex.EncodeToString(encPriv.Bytes()),
		Status:               StatusNew,
		CreatedAt:            now,
	}, nil
}

// Load reads an identity file. Legacy publicKey/privateKey fields are migrated
// in memory; call Save to persist encryption keys and the new field names.
func Load(path string) (Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, err
	}
	id, err := parseIdentity(raw)
	if err != nil {
		return Identity{}, err
	}
	return id, nil
}

type identityFile struct {
	Version              int    `json:"version"`
	ServerURL            string `json:"serverUrl"`
	Name                 string `json:"name"`
	SigningPublicKey     string `json:"signingPublicKey"`
	SigningPrivateKey    string `json:"signingPrivateKey"`
	EncryptionPublicKey  string `json:"encryptionPublicKey"`
	EncryptionPrivateKey string `json:"encryptionPrivateKey"`
	PairingID            string `json:"pairingId,omitempty"`
	PollToken            string `json:"pollToken,omitempty"`
	PrincipalID          string `json:"principalId,omitempty"`
	Status               string `json:"status"`
	CreatedAt            string `json:"createdAt,omitempty"`
	// Legacy field names from the first pairing slice.
	PublicKey  string `json:"publicKey,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"`
}

func parseIdentity(raw []byte) (Identity, error) {
	var f identityFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return Identity{}, fmt.Errorf("identity json: %w", err)
	}
	id := Identity{
		Version:              f.Version,
		ServerURL:            strings.TrimRight(f.ServerURL, "/"),
		Name:                 f.Name,
		SigningPublicKey:     strings.ToLower(f.SigningPublicKey),
		SigningPrivateKey:    f.SigningPrivateKey,
		EncryptionPublicKey:  strings.ToLower(f.EncryptionPublicKey),
		EncryptionPrivateKey: f.EncryptionPrivateKey,
		PairingID:            f.PairingID,
		PollToken:            f.PollToken,
		PrincipalID:          f.PrincipalID,
		Status:               f.Status,
		CreatedAt:            f.CreatedAt,
	}
	if id.SigningPublicKey == "" && f.PublicKey != "" {
		id.SigningPublicKey = strings.ToLower(f.PublicKey)
		id.SigningPrivateKey = f.PrivateKey
	}
	if id.Version == 0 {
		id.Version = identityFileVersion
	}
	if id.Status == "" {
		id.Status = StatusNew
	}
	if err := id.ensureEncryptionKeys(); err != nil {
		return Identity{}, err
	}
	if err := id.validate(); err != nil {
		return Identity{}, err
	}
	return id, nil
}

func (id *Identity) ensureEncryptionKeys() error {
	if id.EncryptionPublicKey != "" && id.EncryptionPrivateKey != "" {
		return nil
	}
	encPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("encryption key: %w", err)
	}
	id.EncryptionPublicKey = hex.EncodeToString(encPriv.PublicKey().Bytes())
	id.EncryptionPrivateKey = hex.EncodeToString(encPriv.Bytes())
	return nil
}

func (id Identity) validate() error {
	if _, err := id.SigningPrivate(); err != nil {
		return err
	}
	if _, err := id.EncryptionPrivate(); err != nil {
		return err
	}
	return nil
}

// SigningPrivate returns the Ed25519 private key.
func (id Identity) SigningPrivate() (ed25519.PrivateKey, error) {
	seed, err := hex.DecodeString(id.SigningPrivateKey)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("invalid signing private key")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := hex.EncodeToString(priv.Public().(ed25519.PublicKey))
	if id.SigningPublicKey != "" && pub != strings.ToLower(id.SigningPublicKey) {
		return nil, fmt.Errorf("signing public key does not match private key")
	}
	return priv, nil
}

// EncryptionPrivate returns the X25519 private key.
func (id Identity) EncryptionPrivate() (*ecdh.PrivateKey, error) {
	raw, err := hex.DecodeString(id.EncryptionPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid encryption private key")
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid encryption private key: %w", err)
	}
	pub := hex.EncodeToString(priv.PublicKey().Bytes())
	if id.EncryptionPublicKey != "" && pub != strings.ToLower(id.EncryptionPublicKey) {
		return nil, fmt.Errorf("encryption public key does not match private key")
	}
	return priv, nil
}

// Save writes the identity with mode 0600 using a temp file + rename.
func Save(path string, id Identity) error {
	if err := id.validate(); err != nil {
		return err
	}
	id.Version = identityFileVersion
	id.SigningPublicKey = strings.ToLower(id.SigningPublicKey)
	id.EncryptionPublicKey = strings.ToLower(id.EncryptionPublicKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadOrCreate loads path or generates and saves a new identity.
func LoadOrCreate(path, serverURL, name string) (Identity, error) {
	id, err := Load(path)
	if err == nil {
		changed := false
		if id.ServerURL == "" && serverURL != "" {
			id.ServerURL = strings.TrimRight(serverURL, "/")
			changed = true
		}
		if id.Name == "" && name != "" {
			id.Name = name
			changed = true
		}
		if changed {
			if err := Save(path, id); err != nil {
				return Identity{}, err
			}
		}
		return id, nil
	}
	if !os.IsNotExist(err) {
		return Identity{}, err
	}
	id, err = Generate(serverURL, name)
	if err != nil {
		return Identity{}, err
	}
	if err := Save(path, id); err != nil {
		return Identity{}, err
	}
	return id, nil
}
