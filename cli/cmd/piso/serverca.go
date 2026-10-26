package main

// Gateway CA bootstrap.
//
// A gateway terminates its own TLS with the private piso CA (see
// gateway/cmd/gateway/main.go: servePublicTLS signs with ca.TLSCertificate), so
// a fresh machine cannot verify the first HTTPS call: the CA is in no system
// trust store. The gateway publishes that CA at /ca.crt on the public allowlist
// (gateway/internal/server/public.go) precisely so a client can bootstrap, but
// the CLI had no way to use it — the only documented fix was installing a
// private CA system-wide as root, on a machine whose whole purpose is to be
// disposable.
//
// So: try verified TLS first (a publicly trusted cert, e.g. Caddy with ACME,
// succeeds and nothing is fetched or pinned). Only on an unknown-authority
// error, fetch /ca.crt, pin it under the worker's state dir, and retry with a
// private pool. Everything afterwards — the pairing POST, the assignment poll,
// every later CLI call — verifies against the pinned CA.
//
// This is trust-on-first-use on exactly one GET. It is not free, and the
// defence is the approval that already gates a pairing: compare the CA
// fingerprint this prints against `piso gateway status` on the gateway host
// before approving. --ca <file> skips the window entirely; --no-ca-fetch
// restores the old behaviour.

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"piso/cli/internal/pisoconfig"
)

// maxCACertBytes bounds /ca.crt. A CA bundle is a few KB; anything larger is
// not a certificate and must not be held in memory.
const maxCACertBytes = 1 << 20

// gatewayCA is how the CLI trusts a gateway whose certificate is signed by the
// private piso CA. A nil *gatewayCA means "use the system roots only", i.e.
// nothing unusual happened and no fetch is allowed.
type gatewayCA struct {
	// PinPath is where a fetched CA was written, empty when nothing was fetched.
	PinPath string
	// SHA256 is the fingerprint of the fetched (or explicitly given) CA's DER.
	SHA256 string
	// Subject is the CA certificate's subject, for the operator to recognise.
	Subject string
	// Fetched reports whether the CA came from the server rather than a flag.
	Fetched bool
	// client is the http client that trusts Pool plus the system roots.
	client *http.Client
}

// caFetchMode is what a command was allowed to do about an untrusted gateway.
type caFetchMode int

const (
	// caFetchAuto fetches and pins /ca.crt when TLS fails on an unknown CA.
	caFetchAuto caFetchMode = iota
	// caFetchOff fails with the manual instructions instead (--no-ca-fetch).
	caFetchOff
)

// errUnknownAuthority reports whether err is TLS refusing a cert it cannot
// chain. Only that error justifies touching the trust store: a hostname
// mismatch, an expired cert, or a connection failure must stay fatal.
func errUnknownAuthority(err error) bool {
	var unknown x509.UnknownAuthorityError
	return errors.As(err, &unknown)
}

// gatewayCAClient returns an http client that trusts whatever this command was
// told to trust, plus the system roots. It probes the server first so the CA is
// fetched before any state-changing request, and so a publicly trusted gateway
// needs nothing at all.
func gatewayCAClient(serverURL, slug, caFile string, mode caFetchMode) (*gatewayCA, error) {
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if caFile != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("--ca: %w", err)
		}
		pool, subject, sum, err := loadCAPool(pemBytes, caFile)
		if err != nil {
			return nil, err
		}
		ca := &gatewayCA{SHA256: sum, Subject: subject, client: poolClient(pool)}
		if err := ca.probe(serverURL); err != nil {
			return nil, fmt.Errorf("--ca %s does not verify %s: %w", caFile, serverURL, err)
		}
		fmt.Printf("piso: gateway CA %s sha256:%s (%s)\n", ca.Subject, ca.SHA256, caFile)
		return ca, nil
	}

	// Verified TLS first. This is the whole story for a public certificate.
	probeErr := (&gatewayCA{client: poolClient(nil)}).probe(serverURL)
	if probeErr == nil {
		return nil, nil
	}
	if !errUnknownAuthority(probeErr) {
		return nil, probeErr
	}

	// A CA pinned by an earlier launch needs no network at all — and keeps
	// working when the gateway is unreachable for /ca.crt but still serving.
	if pin, pemBytes, sum, err := readPinnedCA(slug); err == nil {
		if pool, subject, _, err := loadCAPool(pemBytes, pin); err == nil && (&gatewayCA{client: poolClient(pool)}).probe(serverURL) == nil {
			fmt.Printf("piso: gateway CA %s sha256:%s (pinned at %s)\n", subject, sum, pin)
			return &gatewayCA{PinPath: pin, SHA256: sum, Subject: subject, client: poolClient(pool)}, nil
		}
	}

	if mode == caFetchOff {
		return nil, manualCAError(serverURL, probeErr)
	}

	pemBytes, caPath, err := fetchGatewayCA(serverURL)
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, manualCAHint(serverURL))
	}
	pool, subject, sum, err := loadCAPool(pemBytes, serverURL+caPath)
	if err != nil {
		return nil, err
	}
	ca := &gatewayCA{SHA256: sum, Subject: subject, Fetched: true, client: poolClient(pool)}
	if err := ca.probe(serverURL); err != nil {
		return nil, fmt.Errorf("fetched CA from %s/ca.crt does not verify it: %w", serverURL, err)
	}
	if pin, err := pinGatewayCA(slug, pemBytes); err == nil {
		ca.PinPath = pin
	}
	fmt.Printf("piso: gateway CA %s sha256:%s (fetched from %s%s)\n", subject, sum, serverURL, caPath)
	if ca.PinPath != "" {
		fmt.Printf("piso: pinned at %s\n", ca.PinPath)
	}
	fmt.Println("piso: compare that fingerprint with `piso gateway status` on the gateway host before approving")
	return ca, nil
}

// pairClient is the client the pairing flow uses: the gateway CA (fetched,
// pinned, or supplied) plus the system roots. setPairClient installs it, so the
// helpers below stay usable from tests that never touch TLS.
var pairClient = poolClient(nil)

// setPairClient installs the client used by pairHTTP/pairHTTPPost. nil restores
// the system-roots default.
func setPairClient(ca *gatewayCA) {
	if ca == nil || ca.client == nil {
		pairClient = poolClient(nil)
		return
	}
	pairClient = ca.client
}

func pairHTTP() *http.Client { return pairClient }

func pairHTTPPost(url, contentType string, body io.Reader) (*http.Response, error) {
	return pairClient.Post(url, contentType, body)
}

// probe makes one harmless request so a trust failure surfaces here, where it
// can be handled, instead of inside a pairing POST that may have side effects.
func (c *gatewayCA) probe(serverURL string) error {
	return c.get(serverURL + "/api/v1/health")
}

func (c *gatewayCA) get(url string) error {
	client := c.client
	if client == nil {
		client = poolClient(nil)
	}
	res, err := client.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	return nil
}

// caPEMPaths are the two places a gateway publishes its CA: the worker-facing
// mux serves /ca.crt, the dashboard mux serves /api/v1/ca.pem. Which one a
// given --server reaches depends on how the host is fronted, so try both rather
// than guessing. Both are unauthenticated by design — the CA is not a secret,
// every client needs it to verify the server.
var caPEMPaths = []string{"/ca.crt", "/api/v1/ca.pem"}

// fetchGatewayCA reads the CA without verification. Safe because the result is
// only used to verify the same server on the next request: a wrong CA fails
// that probe, and a substituted one is visible as a fingerprint mismatch to the
// human approving the pairing.
func fetchGatewayCA(serverURL string) ([]byte, string, error) {
	var firstErr error
	for _, path := range caPEMPaths {
		pemBytes, err := fetchPEM(serverURL + path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return pemBytes, path, nil
	}
	return nil, "", firstErr
}

func fetchPEM(url string) ([]byte, error) {
	plain := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // pinned immediately below
		},
	}
	res, err := plain.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxCACertBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	if len(raw) > maxCACertBytes {
		return nil, fmt.Errorf("fetch %s: larger than %d bytes", url, maxCACertBytes)
	}
	if block, _ := pem.Decode(raw); block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("fetch %s: not a PEM certificate", url)
	}
	return raw, nil
}

// loadCAPool parses a PEM bundle and returns a pool with those certificates on
// top of the system roots. from names the source for error messages.
func loadCAPool(pemBytes []byte, from string) (*x509.CertPool, string, string, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, "", "", fmt.Errorf("%s contains no PEM certificates", from)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, "", "", fmt.Errorf("%s holds no certificate block", from)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: %w", from, err)
	}
	sum := sha256.Sum256(cert.Raw)
	return pool, cert.Subject.String(), hex.EncodeToString(sum[:]), nil
}

// pinnedCAPath is where pinGatewayCA writes: next to the worker's identity, so
// every later launch verifies the same gateway without fetching again.
func pinnedCAPath(slug string) (string, error) {
	if strings.TrimSpace(slug) == "" {
		return "", errors.New("no worker name")
	}
	dataDir, err := pisoconfig.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "technocore", "workers", slug, "gateway-ca.crt"), nil
}

// readPinnedCA returns the CA an earlier launch stored for this worker.
func readPinnedCA(slug string) (string, []byte, string, error) {
	path, err := pinnedCAPath(slug)
	if err != nil {
		return "", nil, "", err
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return "", nil, "", err
	}
	sum := ""
	if block, _ := pem.Decode(pemBytes); block != nil {
		d := sha256.Sum256(block.Bytes)
		sum = hex.EncodeToString(d[:])
	}
	return path, pemBytes, sum, nil
}

// pinGatewayCA stores the CA next to the worker's identity so later commands can
// verify the same gateway without fetching again. Best effort: a read-only data
// dir must not fail the install.
func pinGatewayCA(slug string, pemBytes []byte) (string, error) {
	path, err := pinnedCAPath(slug)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pemBytes, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// caFingerprintFile reports the sha256 of the first certificate in a PEM file,
// so an operator can compare it with the fingerprint a worker printed while
// approving a pairing.
func caFingerprintFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

func manualCAError(serverURL string, cause error) error {
	return fmt.Errorf("%w\n%s", cause, manualCAHint(serverURL))
}

// manualCAHint is the second half of the old failure, i.e. the thing the error
// never told anyone.
func manualCAHint(serverURL string) string {
	return fmt.Sprintf(`piso: that gateway signs its own certificate with the piso CA, which this machine does not trust.
piso: either let piso fetch it (drop --no-ca-fetch), or fetch it yourself:
  curl -kf %[1]s/ca.crt -o /tmp/piso-gateway-ca.crt
  piso worker launch --server %[1]s --ca /tmp/piso-gateway-ca.crt`, serverURL)
}

// poolClient builds the client used for every pair-time request. nil pool means
// the system roots alone.
func poolClient(pool *x509.CertPool) *http.Client {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if pool != nil {
		cfg.RootCAs = pool
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: cfg},
	}
}
