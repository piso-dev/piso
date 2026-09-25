package main

import (
	"os"
	"testing"

	"piso/internal/technocore"
)

func TestLoadOrCreateIdentityViaPackage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PISO_DATA", dir)
	path, err := identityPath()
	if err != nil {
		t.Fatal(err)
	}
	id, err := technocore.LoadOrCreate(path, "https://server.example", "box")
	if err != nil {
		t.Fatal(err)
	}
	if id.SigningPublicKey == "" || id.EncryptionPublicKey == "" {
		t.Fatal("expected both keypairs")
	}
	again, err := technocore.LoadOrCreate(path, "https://other", "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if again.SigningPublicKey != id.SigningPublicKey {
		t.Fatal("identity was regenerated")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayStatusUnenrolled(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	if err := cmdGatewayStatus(); err != nil {
		t.Fatal(err)
	}
}
