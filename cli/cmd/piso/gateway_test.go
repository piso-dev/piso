package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"piso/internal/authlink"
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

// An enrollment link is signed by the local identity, so it can only be redeemed
// by the gateway that holds the matching key — not by "any gateway".
func TestEnrollLinkIsBoundToThisGateway(t *testing.T) {
	mint := func(t *testing.T) (technocore.Identity, string, string, int64) {
		t.Helper()
		dir := t.TempDir()
		t.Setenv("PISO_DATA", dir)
		path, err := identityPath()
		if err != nil {
			t.Fatal(err)
		}
		id, err := technocore.LoadOrCreate(path, "https://server.example", "gw")
		if err != nil {
			t.Fatal(err)
		}
		nonce, err := authlink.NewNonce()
		if err != nil {
			t.Fatal(err)
		}
		exp := time.Now().Add(authlink.TTL).Unix()
		sig, err := authlink.Sign(id.SigningPrivateKey, nonce, exp)
		if err != nil {
			t.Fatal(err)
		}
		return id, nonce, sig, exp
	}

	mine, nonce, sig, exp := mint(t)
	other, _, _, _ := mint(t)

	if authlink.Verify(mine.SigningPublicKey, nonce, sig, exp, time.Now()) != nil {
		t.Fatal("the minting gateway must accept its own link")
	}
	// The whole point: a different gateway's identity rejects it.
	if err := authlink.Verify(other.SigningPublicKey, nonce, sig, exp, time.Now()); err == nil {
		t.Fatal("an enrollment link must not be redeemable on another gateway")
	}
	if mine.SigningPublicKey == other.SigningPublicKey {
		t.Fatal("two gateways must not share an identity key")
	}
}

// The link points at the dashboard, never at the control API or a worker URL.
func TestEnrollLinkTargetsDashboardOrigin(t *testing.T) {
	id := technocore.Identity{
		Status:    technocore.StatusActive,
		PublicURL: "https://gateway.example.com", // worker-facing mux, different name
	}
	base, err := dashboardBase(id, "")
	if err != nil {
		t.Fatal(err)
	}
	if base != "https://gateway.example.com" {
		t.Fatalf("base = %q", base)
	}
	// The link itself is rooted there, not at the control API.
	link := authlink.URL(base, "abc", "sig", 1700000000)
	if !strings.HasPrefix(link, "https://gateway.example.com/auth/link?") {
		t.Fatalf("link = %q", link)
	}
	// An explicit dashboard host wins over the public worker URL.
	base, err = dashboardBase(id, "dashboard.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if base != "https://dashboard.example.com" {
		t.Fatalf("explicit --host must win, got %q", base)
	}
}
