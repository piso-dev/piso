package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selfSignedGateway is an HTTPS server whose certificate chains to a CA no
// system trust store knows — exactly what a piso gateway presents.
func selfSignedGateway(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "piso gateway root CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ca.crt":
			_, _ = w.Write(caPEM)
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, caPEM
}

// The bug: a fresh machine cannot verify the gateway, and the CLI refused with a
// bare x509 error. It must now fetch /ca.crt, pin it, and proceed.
func TestGatewayCAClientFetchesAndPins(t *testing.T) {
	srv, caPEM := selfSignedGateway(t)
	dir := t.TempDir()
	t.Setenv("PISO_DATA", dir)

	ca, err := gatewayCAClient(srv.URL, "worker-1", "", caFetchAuto)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if ca == nil || !ca.Fetched {
		t.Fatalf("expected a fetched CA, got %+v", ca)
	}
	if ca.SHA256 == "" {
		t.Fatal("fingerprint missing")
	}
	if !strings.Contains(ca.Subject, "piso gateway root CA") {
		t.Fatalf("subject = %q", ca.Subject)
	}
	want := filepath.Join(dir, "technocore", "workers", "worker-1", "gateway-ca.crt")
	if ca.PinPath != want {
		t.Fatalf("pin = %q, want %q", ca.PinPath, want)
	}
	if got, err := os.ReadFile(want); err != nil {
		t.Fatal(err)
	} else if string(got) != string(caPEM) {
		t.Fatal("pinned CA differs from what /ca.crt served")
	}
	// The client it returned must actually verify the server.
	if err := ca.probe(srv.URL); err != nil {
		t.Fatalf("probe with fetched CA: %v", err)
	}
	// And that same client is what the pairing flow now uses.
	setPairClient(ca)
	defer setPairClient(nil)
	res, err := pairHTTP().Get(srv.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("pair client cannot reach the gateway: %v", err)
	}
	res.Body.Close()
}

// --ca skips the fetch entirely and never pins.
func TestGatewayCAClientUsesExplicitCA(t *testing.T) {
	srv, caPEM := selfSignedGateway(t)
	t.Setenv("PISO_DATA", t.TempDir())
	file := filepath.Join(t.TempDir(), "gateway-ca.crt")
	if err := os.WriteFile(file, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	ca, err := gatewayCAClient(srv.URL, "worker-1", file, caFetchAuto)
	if err != nil {
		t.Fatalf("explicit CA: %v", err)
	}
	if ca == nil || ca.Fetched {
		t.Fatalf("expected an explicit (not fetched) CA, got %+v", ca)
	}
	if ca.PinPath != "" {
		t.Fatalf("--ca must not pin: %q", ca.PinPath)
	}
}

// --no-ca-fetch restores the old failure, but must now explain the fix.
func TestGatewayCAClientNoFetchExplainsFix(t *testing.T) {
	srv, _ := selfSignedGateway(t)
	t.Setenv("PISO_DATA", t.TempDir())
	_, err := gatewayCAClient(srv.URL, "worker-1", "", caFetchOff)
	if err == nil {
		t.Fatal("expected a trust error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "/ca.crt") || !strings.Contains(msg, "--ca ") {
		t.Fatalf("error does not explain the fix: %v", err)
	}
}

// A CA that already verifies must be classified as "not an unknown authority",
// so the fetch path is never entered for it.
func TestErrUnknownAuthorityClassification(t *testing.T) {
	srv, _ := selfSignedGateway(t)
	srvErr := (&gatewayCA{}).probe(srv.URL)
	if srvErr == nil {
		t.Fatal("a private CA must not verify against system roots")
	}
	if !errUnknownAuthority(srvErr) {
		t.Fatalf("expected unknown-authority classification, got %v", srvErr)
	}
	if errUnknownAuthority(nil) || errUnknownAuthority(os.ErrNotExist) {
		t.Fatal("unrelated errors must not be treated as trust failures")
	}
}

// A second launch reuses the pin, so it needs neither /ca.crt nor a flag — and
// works even with fetching disabled.
func TestGatewayCAClientReusesPin(t *testing.T) {
	srv, _ := selfSignedGateway(t)
	t.Setenv("PISO_DATA", t.TempDir())
	if _, err := gatewayCAClient(srv.URL, "worker-1", "", caFetchAuto); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	ca, err := gatewayCAClient(srv.URL, "worker-1", "", caFetchOff)
	if err != nil {
		t.Fatalf("second launch should reuse the pin: %v", err)
	}
	if ca == nil || ca.Fetched {
		t.Fatalf("expected a pinned (not fetched) CA, got %+v", ca)
	}
	if ca.PinPath == "" {
		t.Fatal("pinned CA path missing")
	}
}

// A dashboard-hosted gateway serves the CA at /api/v1/ca.pem, not /ca.crt.
func TestGatewayCAClientFallsBackToDashboardCAPath(t *testing.T) {
	srv, caPEM := selfSignedGateway(t) // serves /ca.crt
	// Re-register the handler so only the dashboard path answers.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ca.pem" {
			_, _ = w.Write(caPEM)
			return
		}
		http.NotFound(w, r)
	})
	t.Setenv("PISO_DATA", t.TempDir())
	ca, err := gatewayCAClient(srv.URL, "worker-1", "", caFetchAuto)
	if err != nil {
		t.Fatalf("dashboard CA path: %v", err)
	}
	if ca == nil || !ca.Fetched {
		t.Fatalf("expected a fetched CA, got %+v", ca)
	}
}
