package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"piso/internal/authlink"
	"piso/internal/hostauth"
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

// The CLI and the gateway are two halves of one contract: the CLI signs a
// host-authenticated request, the gateway verifies it. Nothing in either package
// can see the other (gateway/internal is out of reach from cli), so the body
// binding in particular is only ever tested end to end by making the CLI talk to
// a verifier here.
func TestAddNostrKeySignsWhatTheGatewayVerifies(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PISO_DATA", dir)
	path, err := identityPath()
	if err != nil {
		t.Fatal(err)
	}
	id, err := technocore.LoadOrCreate(path, "https://gateway.example.com", "gw")
	if err != nil {
		t.Fatal(err)
	}

	var (
		gotPub    string
		gotLabel  string
		verifiers int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifiers++
		if r.URL.Path != "/api/v1/auth/nostr-keys" || r.Method != http.MethodPost {
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
		if err := hostauth.Verify(id.SigningPublicKey, r, r.URL.RequestURI(), time.Now()); err != nil {
			t.Errorf("signature: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		var body struct {
			Pubkey string `json:"pubkey"`
			Label  string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body was not readable after verification: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotPub, gotLabel = body.Pubkey, body.Label
		io.WriteString(w, `{"added":true,"id":"cred-1","kind":"nostr","pubkey":"`+body.Pubkey+`"}`)
	}))
	defer srv.Close()
	t.Setenv("PISO_GATEWAY", srv.URL)

	const npub = "npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptg"
	const wantHex = "7e7e9c42a91bfef19fa929e5fda1b72e0ebc1a4c1141673e2794234d86addf4e"

	if err := cmdGatewayAuthAddNostr([]string{"--label", "fleet", npub}); err != nil {
		t.Fatalf("add-nostr: %v", err)
	}
	if verifiers != 1 {
		t.Fatalf("want one call, got %d", verifiers)
	}
	// The gateway only ever sees the hex form, whatever the operator pasted.
	if gotPub != wantHex {
		t.Fatalf("pubkey sent = %q, want %q", gotPub, wantHex)
	}
	if gotLabel != "fleet" {
		t.Fatalf("label = %q", gotLabel)
	}

	// Several keys in one invocation is one request each, so a single bad key
	// fails before anything is added rather than half-way through.
	before := verifiers
	err = cmdGatewayAuthAddNostr([]string{wantHex, "nonsense"})
	if err == nil {
		t.Fatal("want an error for the bad key")
	}
	if verifiers != before {
		t.Fatalf("a bad key must be refused before any request is sent (sent %d)", verifiers-before)
	}

	// A valid run with two keys sends two signed requests.
	if err := cmdGatewayAuthAddNostr([]string{wantHex, wantHex}); err != nil {
		t.Fatal(err)
	}
	if got := verifiers - before; got != 2 {
		t.Fatalf("want 2 calls, got %d", got)
	}

	// With no identity there is nothing to sign with, and saying so beats a
	// confusing HTTP error.
	t.Setenv("PISO_DATA", t.TempDir())
	if err := cmdGatewayAuthAddNostr([]string{wantHex}); err == nil {
		t.Fatal("want an error without an identity")
	}
}
