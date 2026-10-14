// Command gateway runs the piso MITM gateway: egress proxy (CONNECT TLS
// termination + substitution/blocking), the control-plane web UI + API, and
// the ingress reverse proxy for name.piso.local routes.
//
// Flags follow the convention: everything is overridable by the CLI/compose.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"piso/gateway/internal/proxy"
	"piso/gateway/internal/server"
	"piso/gateway/internal/store"
	"piso/gateway/internal/technocore"
	"piso/internal/mcpchannel"
	tc "piso/internal/technocore"
)

// workerIdentityFn returns the proxy's request→worker-identity callback. It
// resolves the request's origin IP against the worker registry (populated by
// the host CLI at `piso up`) and returns the worker slug, falling back to the
// container name or, failing that, "worker".
func workerIdentityFn(st *store.Store) func(*http.Request) string {
	return func(r *http.Request) string {
		if tok := headerBearer(r); tok != "" {
			sum := sha256.Sum256([]byte(tok))
			if rec, ok := st.WorkerByTokenHash(hex.EncodeToString(sum[:])); ok {
				return rec.Slug
			}
		}
		slug, ok := st.WorkerByIP(remoteIP(r))
		if ok {
			return slug.Slug
		}
		return ""
	}
}

func headerBearer(r *http.Request) string {
	for _, h := range []string{r.Header.Get("Authorization"), r.Header.Get("Proxy-Authorization")} {
		h = strings.TrimSpace(h)
		if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
			return strings.TrimSpace(h[7:])
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Piso-Worker-Token"))
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	return host
}

func main() {
	var (
		statePath      = flag.String("state", envOr("PISO_STATE_FILE", ".piso/state.json"), "state file (secrets/rules/domains/exceptions/routes)")
		patternsPath   = flag.String("patterns", envOr("PISO_PATTERNS_FILE", ".piso/patterns.json"), "credential pattern library file")
		logPath        = flag.String("log", envOr("PISO_LOG_FILE", ".piso/requests.jsonl"), "request log (JSONL)")
		activitiesPath = flag.String("activities", envOr("PISO_ACTIVITIES_FILE", ".piso/activities.db"), "activity database (SQLite)")
		caCertPath     = flag.String("ca-cert", envOr("PISO_CA_CERT", ".piso/ca.crt"), "CA certificate (generated if missing)")
		caKeyPath      = flag.String("ca-key", envOr("PISO_CA_KEY", ".piso/ca.key"), "CA private key (generated if missing)")
		proxyAddr      = flag.String("proxy-listen", envOr("PISO_PROXY_LISTEN", ":8080"), "egress proxy listen addr")
		ctrlAddr       = flag.String("ctrl-listen", envOr("PISO_CTRL_LISTEN", ":8081"), "control plane (UI+API) listen addr")
		workerAddr     = flag.String("worker-listen", envOr("PISO_WORKER_LISTEN", ":8083"), "worker API listen addr (vpc only)")
		publicAddr     = flag.String("public-listen", envOr("PISO_PUBLIC_LISTEN", ":8443"), "TLS mux for remote workers (CONNECT + worker API)")
		ingressAddr    = flag.String("ingress-listen", envOr("PISO_INGRESS_LISTEN", ":8082"), "ingress reverse proxy listen addr")
		maxLog         = flag.Int("max-log", 5000, "max in-memory log records")
	)
	flag.Parse()

	if err := os.MkdirAll(".piso", 0o700); err != nil {
		log.Fatalf("mkdir: %v", err)
	}

	st, err := store.New(*statePath, *logPath, *patternsPath, *activitiesPath, *maxLog)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	dataDir := filepath.Dir(*statePath)
	if err := ensureGatewayIdentity(dataDir, *statePath); err != nil {
		log.Fatalf("identity: %v", err)
	}

	pat, err := st.LoadPatterns()
	if err != nil {
		log.Fatalf("patterns: %v", err)
	}

	ca, err := proxy.LoadCA(*caCertPath, *caKeyPath)
	if err != nil {
		log.Fatalf("ca: %v", err)
	}

	h := proxy.New(ca, st, pat, workerIdentityFn(st))
	srv := server.New(st, pat, h, ca)
	srv.DataDir = filepath.Dir(*statePath)
	go technocore.Run(st, srv.DataDir)
	// The Telegram front door polls only while a bot token is stored; with none
	// it idles, so this is unconditional.
	go srv.Telegram.Run(context.Background())

	// WebHandler is the single host web entrypoint (dashboard + ingress on the
	// same port, dispatched by Host). The old ControlHandler is kept for tests;
	// IngressHandler is kept on the ingress port for backward compatibility.
	go serve("web", *ctrlAddr, srv.WebHandler())
	go serve("worker-api", *workerAddr, srv.WorkerHandler())
	go serve("ingress", *ingressAddr, srv.IngressHandler())
	go serve("egress-proxy", *proxyAddr, h)
	id, _ := tc.Load(tc.Path(srv.DataDir))
	if id.PlacementOrLocal() == tc.PlacementStandalone {
		host := publicHostname(id.PublicURL)
		go servePublicTLS("public-workers", *publicAddr, host, ca, srv.PublicWorkerHandler())
		if id.Status == tc.StatusActive {
			// Pool stays up after a public URL is set until every assigned
			// remote worker has checked in on that URL.
			go technocore.RunTunnel(id, srv.DataDir, srv.PublicWorkerHandler(), srv.RelayStillNeeded)
		}
	}
	select {}
}

func publicHostname(publicURL string) string {
	publicURL = strings.TrimSpace(publicURL)
	if publicURL == "" {
		return "piso-gateway"
	}
	if !strings.Contains(publicURL, "://") {
		publicURL = "https://" + publicURL
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Hostname() == "" {
		return "piso-gateway"
	}
	return u.Hostname()
}

func servePublicTLS(name, addr, hostname string, ca *proxy.CA, h http.Handler) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("%s: listen %s: %v", name, addr, err)
		return
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			host := chi.ServerName
			if host == "" {
				host = hostname
			}
			cert, err := ca.TLSCertificate(host)
			if err != nil {
				return nil, err
			}
			return &cert, nil
		},
	}
	log.Printf("%s listening on %s (tls %s)", name, addr, hostname)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, TLSConfig: cfg}
	if err := srv.Serve(tls.NewListener(ln, cfg)); err != nil {
		log.Printf("%s: %v", name, err)
	}
}

func serve(name, addr string, h http.Handler) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("%s: listen %s: %v", name, addr, err)
	}
	log.Printf("%s listening on %s", name, addr)
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.Serve(l); err != nil {
		log.Fatalf("%s: %v", name, err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ensureGatewayIdentity creates identity.json when it is missing and publishes
// gateway.json into every workers/<slug>/ directory. A corrupt file is fatal:
// never overwrite it.
func ensureGatewayIdentity(dataDir, statePath string) error {
	id, err := tc.LoadOrCreate(tc.Path(dataDir), "", "")
	if err != nil {
		return err
	}
	enc, err := hex.DecodeString(id.EncryptionPublicKey)
	if err != nil {
		return err
	}
	sign, err := hex.DecodeString(id.SigningPublicKey)
	if err != nil {
		return err
	}
	pin := mcpchannel.PinFromPublic(enc, sign)
	if err := store.PublishGatewayPin(statePath, pin); err != nil {
		return err
	}
	log.Printf("technocore: identity %s (%s) key %s", id.Status, id.Fingerprint(), pin.KeyID)
	return nil
}
