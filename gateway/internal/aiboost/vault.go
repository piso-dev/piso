// Package aiboost owns the gateway's delegated AI Boost connection. No token
// from this package is published as a worker placeholder or shared secret.
package aiboost

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

type record struct {
	BaseURL        string    `json:"baseUrl"`
	RedirectURI    string    `json:"redirectUri"`
	ClientID       string    `json:"clientId"`
	ClientSecret   string    `json:"clientSecret"`
	AccessToken    string    `json:"accessToken,omitempty"`
	RefreshToken   string    `json:"refreshToken,omitempty"`
	AccessExpires  time.Time `json:"accessExpires,omitempty"`
	RefreshExpires time.Time `json:"refreshExpires,omitempty"`
	Status         string    `json:"status"`
	// Persisted before sending a single-use refresh. A crash or ambiguous
	// network response must never cause the consumed token to be retried.
	RefreshPending bool `json:"refreshPending,omitempty"`
}

type vault struct {
	dir  string
	aead cipher.AEAD
}

func openVault(dir string) (*vault, record, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, record{}, err
	}
	keyPath := filepath.Join(dir, "aiboost.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		// Never silently replace the key for existing ciphertext.
		if _, e := os.Stat(filepath.Join(dir, "aiboost.enc")); !errors.Is(e, os.ErrNotExist) {
			return nil, record{}, errors.New("AI Boost vault key missing")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, record{}, err
		}
		if err = durableWrite(keyPath, key); err != nil {
			return nil, record{}, err
		}
	} else if err != nil {
		return nil, record{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, record{}, errors.New("AI Boost vault key invalid")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, record{}, err
	}
	v := &vault{dir: dir, aead: aead}
	raw, err := os.ReadFile(filepath.Join(dir, "aiboost.enc"))
	if errors.Is(err, os.ErrNotExist) {
		return v, record{Status: "none"}, nil
	}
	if err != nil {
		return nil, record{}, err
	}
	n := aead.NonceSize()
	if len(raw) < n {
		return nil, record{}, errors.New("AI Boost vault corrupt")
	}
	plain, err := aead.Open(nil, raw[:n], raw[n:], []byte("piso-aiboost-v1"))
	if err != nil {
		return nil, record{}, errors.New("AI Boost vault authentication failed")
	}
	var r record
	if json.Unmarshal(plain, &r) != nil {
		return nil, record{}, errors.New("AI Boost vault corrupt")
	}
	return v, r, nil
}
func (v *vault) save(r record) error {
	plain, err := json.Marshal(r)
	if err != nil {
		return err
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	raw := v.aead.Seal(nonce, nonce, plain, []byte("piso-aiboost-v1"))
	return durableWrite(filepath.Join(v.dir, "aiboost.enc"), raw)
}
func durableWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".aiboost-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
