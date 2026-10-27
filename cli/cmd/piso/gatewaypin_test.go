package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"piso/internal/mcpchannel"
)

// testPin builds a valid gateway pin the way the gateway does, so the fixture
// cannot drift from mcpchannel.Validate.
func testPin(seed byte) mcpchannel.Pin {
	x := make([]byte, mcpchannel.EncLen)
	s := make([]byte, 32)
	for i := range x {
		x[i] = seed + byte(i)
	}
	for i := range s {
		s[i] = seed*3 + byte(i)
	}
	return mcpchannel.PinFromPublic(x, s)
}

func pinServer(t *testing.T, pin mcpchannel.Pin, status int) *httptest.Server {
	t.Helper()
	return pinServerAuth(t, pin, status, "")
}

// pinServerAuth mirrors the real gateway: on a public listener /mcp/key sits
// behind the remote-worker allowlist, so without the worker's bearer token it is
// a 401 and no pin is served.
func pinServerAuth(t *testing.T, pin mcpchannel.Pin, status int, wantToken string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/key", func(w http.ResponseWriter, r *http.Request) {
		if wantToken != "" && r.Header.Get("Authorization") != "Bearer "+wantToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":     pin.Version,
			"x25519":      pin.X25519,
			"keyId":       pin.KeyID,
			"signing":     pin.Signing,
			"status":      "active",
			"fingerprint": "05f13ce5211c",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A ship provisioned without gateway.json is the bug this fixes: the launcher
// must leave a pin where the container's /etc/piso mount will show it.
func TestInstallWorkerGatewayPinWritesMissingPin(t *testing.T) {
	pin := testPin(1)
	srv := pinServer(t, pin, http.StatusOK)
	dir := t.TempDir()

	out, err := installWorkerGatewayPin(srv.URL, "", dir)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !out.Changed {
		t.Fatal("expected a first install to write the pin")
	}
	if out.Pin.KeyID != pin.KeyID {
		t.Fatalf("keyId = %q, want %q", out.Pin.KeyID, pin.KeyID)
	}
	if out.Fingerprint != "05f13ce5211c" {
		t.Fatalf("fingerprint = %q", out.Fingerprint)
	}

	// The container reads <data>/workers/<slug>/gateway.json through /etc/piso.
	raw, err := os.ReadFile(filepath.Join(dir, mcpchannel.PinFile))
	if err != nil {
		t.Fatalf("pin not written: %v", err)
	}
	onDisk, err := mcpchannel.ParsePin(raw)
	if err != nil {
		t.Fatalf("written pin does not parse: %v", err)
	}
	if onDisk.KeyID != pin.KeyID {
		t.Fatalf("on-disk keyId = %q", onDisk.KeyID)
	}
	if fi, err := os.Stat(filepath.Join(dir, mcpchannel.PinFile)); err == nil && fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644 (the worker mounts it read-only)", fi.Mode().Perm())
	}
}

// A relaunch on an unchanged gateway must not rewrite the file under a running
// container, and must not report a change it did not make.
func TestInstallWorkerGatewayPinLeavesSamePinAlone(t *testing.T) {
	pin := testPin(2)
	srv := pinServer(t, pin, http.StatusOK)
	dir := t.TempDir()

	if _, err := installWorkerGatewayPin(srv.URL, "", dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, mcpchannel.PinFile)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	out, err := installWorkerGatewayPin(srv.URL, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Changed {
		t.Fatal("rewrote an unchanged pin")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("modification time moved on a no-op install")
	}
}

// A re-keyed gateway is the one case a launcher should replace the pin, and it
// has to say so loudly enough that the operator can see the keyId change.
func TestInstallWorkerGatewayPinReplacesReKeyedPin(t *testing.T) {
	oldPin, newPin := testPin(3), testPin(4)
	dir := t.TempDir()
	if err := mcpchannel.WritePin(filepath.Join(dir, mcpchannel.PinFile), oldPin); err != nil {
		t.Fatal(err)
	}
	srv := pinServer(t, newPin, http.StatusOK)

	out, err := installWorkerGatewayPin(srv.URL, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Changed {
		t.Fatal("expected the re-keyed pin to be replaced")
	}
	if out.Replaced != oldPin.KeyID {
		t.Fatalf("replaced = %q, want %q", out.Replaced, oldPin.KeyID)
	}
	got, err := mcpchannel.LoadPin(filepath.Join(dir, mcpchannel.PinFile))
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyID != newPin.KeyID {
		t.Fatalf("on-disk keyId = %q, want %q", got.KeyID, newPin.KeyID)
	}
}

// A gateway too old to serve /mcp/key, or one that answers with something that
// is not a pin, must fail with a reason rather than leave a corrupt file that
// breaks every later piso-mcp call.
func TestInstallWorkerGatewayPinReportsBadResponses(t *testing.T) {
	dir := t.TempDir()
	for name, status := range map[string]int{
		"not found":    http.StatusNotFound,
		"gateway down": http.StatusServiceUnavailable,
	} {
		srv := pinServer(t, testPin(5), status)
		if _, err := installWorkerGatewayPin(srv.URL, "", dir); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if _, err := os.Stat(filepath.Join(dir, mcpchannel.PinFile)); !os.IsNotExist(err) {
			t.Fatalf("%s: a failed fetch left a pin file behind", name)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not a pin</html>"))
	}))
	defer srv.Close()
	if _, err := installWorkerGatewayPin(srv.URL, "", dir); err == nil {
		t.Fatal("expected a parse error")
	}
	if _, err := os.Stat(filepath.Join(dir, mcpchannel.PinFile)); !os.IsNotExist(err) {
		t.Fatal("an unparseable response left a pin file behind")
	}
}

func TestFetchGatewayPinNeedsAnEndpoint(t *testing.T) {
	if _, _, err := fetchGatewayPin("   ", ""); err == nil {
		t.Fatal("expected an error for an empty endpoint")
	}
}

// The public listener authenticates /mcp/key with the worker's own token; an
// anonymous fetch is a 401 and installs nothing. This is the failure that left a
// re-launched ship without a pin.
func TestInstallWorkerGatewayPinSendsWorkerToken(t *testing.T) {
	pin := testPin(7)
	srv := pinServerAuth(t, pin, http.StatusOK, "ship-token-abc")
	dir := t.TempDir()

	// No token: the gateway refuses, and the error must say so rather than
	// blaming the network.
	if _, err := installWorkerGatewayPin(srv.URL, "", dir); err == nil {
		t.Fatal("expected a 401 without the worker token")
	} else if !strings.Contains(err.Error(), "token") {
		t.Fatalf("error does not mention the token: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, mcpchannel.PinFile)); !os.IsNotExist(err) {
		t.Fatal("a rejected fetch left a pin file behind")
	}

	// The relaunch path: token comes from the ship's own token file.
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("ship-token-abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := installWorkerGatewayPin(srv.URL, "", dir)
	if err != nil {
		t.Fatalf("install with token file: %v", err)
	}
	if !out.Changed || out.Pin.KeyID != pin.KeyID {
		t.Fatalf("pin not installed: %+v", out)
	}
}
