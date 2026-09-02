package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

// CA is the MITM certificate authority: a root CA whose cert the worker (and
// host browser) trusts, used to mint per-hostname leaf certs on the fly.
type CA struct {
	cert *x509.Certificate
	key  *rsa.PrivateKey
	der  []byte

	mu    sync.Mutex
	cache map[string]leafEntry // hostname -> leaf cert + private key
}

type leafEntry struct {
	der []byte
	key *rsa.PrivateKey
}

// LoadCA loads the root CA from PEM files, or generates a new one and writes
// it (so it persists across restarts). certFile/keyFile must be writable when
// generating.
func LoadCA(certFile, keyFile string) (*CA, error) {
	certPEM, err1 := os.ReadFile(certFile)
	keyPEM, err2 := os.ReadFile(keyFile)
	if err1 == nil && err2 == nil {
		ca, err := parseCA(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		return ca, nil
	}
	// generate
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "piso gateway root CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	ca := &CA{cert: cert, key: key, der: der, cache: map[string]leafEntry{}}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	return ca, nil
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("no cert PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("no key PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("CA key is not RSA")
	}
	ca := &CA{cert: cert, key: rsaKey, der: certBlock.Bytes, cache: map[string]leafEntry{}}
	return ca, nil
}

// Leaf returns a TLS certificate for hostname, minted from the CA and cached.
func (c *CA) Leaf(hostname string) (tlsCert, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if le, ok := c.cache[hostname]; ok {
		return tlsCert{Certificate: [][]byte{le.der, c.der}, PrivateKey: le.key}, nil
	}
	now := time.Now()
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(0, 3, 0), // 90 days
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(hostname); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{hostname}
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tlsCert{}, err
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &leafKey.PublicKey, c.key)
	if err != nil {
		return tlsCert{}, err
	}
	c.cache[hostname] = leafEntry{der: der, key: leafKey}
	return tlsCert{Certificate: [][]byte{der, c.der}, PrivateKey: leafKey}, nil
}

// CertPEM returns the root CA certificate PEM (for the web UI / CLI to
// distribute to workers).
func (c *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.der})
}