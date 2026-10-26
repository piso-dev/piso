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

func TestErrIfStandaloneGatewayHost(t *testing.T) {
	if err := errIfStandaloneGatewayHost(technocore.Identity{}, os.ErrNotExist); err != nil {
		t.Fatalf("missing identity: %v", err)
	}
	local := technocore.Identity{Status: technocore.StatusActive, Placement: technocore.PlacementLocal}
	if err := errIfStandaloneGatewayHost(local, nil); err != nil {
		t.Fatalf("laptop gateway should allow worker launch: %v", err)
	}
	standalone := technocore.Identity{Status: technocore.StatusActive, Placement: technocore.PlacementStandalone}
	if err := errIfStandaloneGatewayHost(standalone, nil); err == nil {
		t.Fatal("expected refuse on standalone gateway host")
	}
}

func TestResolvePlacementFlag(t *testing.T) {
	p, set, err := resolvePlacementFlag(true, "")
	if err != nil || !set || p != technocore.PlacementStandalone {
		t.Fatalf("standalone alias: %q %v %v", p, set, err)
	}
	p, set, err = resolvePlacementFlag(false, "standalone")
	if err != nil || !set || p != technocore.PlacementStandalone {
		t.Fatalf("placement flag: %q %v %v", p, set, err)
	}
	_, set, err = resolvePlacementFlag(false, "")
	if err != nil || set {
		t.Fatalf("unset: set=%v err=%v", set, err)
	}
	if _, _, err := resolvePlacementFlag(true, "local"); err == nil {
		t.Fatal("expected conflict")
	}
	if _, _, err := resolvePlacementFlag(false, "cloud"); err == nil {
		t.Fatal("expected invalid placement")
	}
}

func TestGatewayStatusUnenrolled(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	if err := cmdGatewayStatus(); err != nil {
		t.Fatal(err)
	}
}

// A bare hostname parses as a URL path, so net/http fails with
// `unsupported protocol scheme ""` and the value is then stuck in
// identity.json (LoadOrCreate only fills an empty serverUrl).
func TestNormalizeServerURL(t *testing.T) {
	for raw, want := range map[string]string{
		"":                              "",
		"api.armortron9000.com":         "https://api.armortron9000.com",
		"api.armortron9000.com/":        "https://api.armortron9000.com",
		" https://api.example.com/ ":    "https://api.example.com",
		"http://127.0.0.1:8000":         "http://127.0.0.1:8000",
		"https://workers.example.com/x": "https://workers.example.com/x",
	} {
		if got := normalizeServerURL(raw); got != want {
			t.Errorf("normalizeServerURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
