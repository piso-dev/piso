package technocore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	id, err := Generate("https://example.com/", "box")
	if err != nil {
		t.Fatal(err)
	}
	if id.ServerURL != "https://example.com" {
		t.Fatalf("server url trim: %q", id.ServerURL)
	}
	if len(id.Fingerprint()) != 12 {
		t.Fatalf("fingerprint %q", id.Fingerprint())
	}
	if err := Save(path, id); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v", st.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SigningPublicKey != id.SigningPublicKey || got.EncryptionPublicKey != id.EncryptionPublicKey {
		t.Fatal("keys changed")
	}
	if _, err := got.SigningPrivate(); err != nil {
		t.Fatal(err)
	}
	if _, err := got.EncryptionPrivate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMigratesLegacyFieldsAndAddsEncryptionKeys(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	fresh, err := Generate("https://s", "n")
	if err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{
  "serverUrl": "https://s",
  "name": "n",
  "publicKey": "` + fresh.SigningPublicKey + `",
  "privateKey": "` + fresh.SigningPrivateKey + `",
  "status": "active"
}`)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SigningPublicKey != fresh.SigningPublicKey {
		t.Fatal("signing key not migrated")
	}
	if got.EncryptionPublicKey == "" || got.EncryptionPrivateKey == "" {
		t.Fatal("encryption keys not created")
	}
}

func TestLoadOrCreateIsStable(t *testing.T) {
	path := Path(t.TempDir())
	a, err := LoadOrCreate(path, "https://s", "n")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(path, "https://other", "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if a.SigningPublicKey != b.SigningPublicKey {
		t.Fatal("regenerated")
	}
}

func TestMismatchedSigningKeyRejected(t *testing.T) {
	id, err := Generate("https://s", "n")
	if err != nil {
		t.Fatal(err)
	}
	id.SigningPublicKey = "00" + id.SigningPublicKey[2:]
	if err := id.validate(); err == nil {
		t.Fatal("expected mismatch error")
	}
}
