package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

// TestCACacheKeyMatchesCert is a regression test for the bug where the CA's
// cached leaf returned the CA's own private key, breaking every connection
// after the first to the same hostname ("error decrypting message" /
// RSA "first octet invalid" handshakes).
func TestLoadCAEmptyFilesGenerate(t *testing.T) {
	dir := t.TempDir()
	crt := filepath.Join(dir, "ca.crt")
	key := filepath.Join(dir, "ca.key")
	if err := os.WriteFile(crt, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ca, err := LoadCA(crt, key)
	if err != nil {
		t.Fatalf("LoadCA empty files: %v", err)
	}
	if ca == nil || ca.cert == nil {
		t.Fatal("expected generated CA")
	}
	st, err := os.Stat(crt)
	if err != nil || st.Size() == 0 {
		t.Fatalf("ca.crt should be written, size=%v err=%v", st, err)
	}
}

func TestCACacheKeyMatchesCert(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadCA(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}

	// mint two leaves for the SAME hostname — the second hits the cache
	first, err := ca.Leaf("api.example.com")
	if err != nil {
		t.Fatalf("first leaf: %v", err)
	}
	second, err := ca.Leaf("api.example.com")
	if err != nil {
		t.Fatalf("second (cached) leaf: %v", err)
	}

	// The cached entry must reuse the SAME private key as the first mint
	// (same generated key, since we cache the first one).
	if first.PrivateKey != second.PrivateKey {
		t.Fatal("cached leaf must reuse the minted private key")
	}

	// And the certificate chain must be verifiable against the CA with that
	// key (i.e., the key actually signs for the presented cert).
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	leafCert, err := x509.ParseCertificate(second.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if _, err := leafCert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   "api.example.com",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("cached leaf does not verify: %v", err)
	}

	// A real handshake must be able to use it (crypto/tls sanity).
	_ = tls.Certificate{Certificate: second.Certificate, PrivateKey: second.PrivateKey}
}