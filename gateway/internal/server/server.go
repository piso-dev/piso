package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"piso/gateway/internal/aiboost"
	"piso/gateway/internal/model"
	"piso/gateway/internal/patterns"
	"piso/gateway/internal/proxy"
	"piso/gateway/internal/store"
	"piso/gateway/internal/technocore"
	"piso/gateway/internal/telegram"
)

//go:embed ui/index.html ui/favicon.svg
var uiFS embed.FS

// Server bundles the control plane + ingress into http.Handlers.
type Server struct {
	Store    *store.Store
	Patterns *patterns.Compiled
	Proxy    *proxy.Handler
	CA       *proxy.CA
	// DataDir is the host-mounted gateway data directory (identity.json).
	DataDir string
	// HTTP is used for MCP OAuth discovery/DCR/token (tests inject).
	HTTP *http.Client
	// DenyPeer overrides worker-vpc detection on the control plane (tests).
	DenyPeer          denyPeerFunc
	githubOAuthMu     sync.Mutex
	githubOAuthStates map[string]githubOAuthAttempt
	boostMu           sync.Mutex
	// Telegram is the Telegram front door: the poller, the update cursor and
	// the allow-list. It owns the only connection to Telegram; nothing else in
	// the gateway talks to that API.
	Telegram     *telegram.Manager
	boost        *aiboost.Client
	boostTickets map[string]time.Time

	// directMu guards workers that have checked in on the public URL.
	// Not persisted: a gateway restart must see a fresh direct check-in
	// before it drops the relay pool.
	directMu     sync.Mutex
	directSwitch map[string]bool // worker name → reached publicUrl
}

// New assembles the server.
func New(st *store.Store, pat *patterns.Compiled, pr *proxy.Handler, ca *proxy.CA) *Server {
	s := &Server{Store: st, Patterns: pat, Proxy: pr, CA: ca}
	// The manager is created here but does not run until main starts it, so a
	// Server in a test is inert: nothing reaches out to Telegram on its own.
	s.Telegram = telegram.NewManager(st, nil)
	s.attachTelegramMailbox()
	return s
}

// revokeOrDelete removes a local secret. A replicated secret is tombstoned
// rather than forgotten: a peer still holding a copy would otherwise publish it
// back. A local-only secret has no peer to convince, so it is deleted.
func (s *Server) revokeOrDelete(rec store.SecretRec) error {
	if rec.TechnocoreID != "" {
		v, err := technocore.Revoke(s.dataDir(), rec.TechnocoreID, rec.ContentVersion)
		if err != nil {
			log.Printf("secrets: revoke %s: %v", rec.ID, err)
		} else if err := s.Store.RevokeSecret(rec.ID, v); err == nil {
			return nil
		}
	}
	return s.Store.DeleteSecret(rec.ID)
}

// ControlHandler returns the host control-plane mux (UI + API). Worker
// peers on the internal vpc are rejected; they use WorkerHandler. Kept for
// API/tests; the production host web entrypoint is WebHandler.
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	api := http.NewServeMux()
	s.routes(api)
	mux.Handle("/api/", api)
	mux.HandleFunc("/", s.ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.denyControlPeer(r.RemoteAddr) {
			writeJSON(w, 403, map[string]string{"error": "control plane is host-only"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/oauth/ai-boost/") {
			s.handleBoostOAuth(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// WebHandler is the single host web entrypoint (Option B): control plane and
// ingress share ONE published port (default 80) and are dispatched by the
// Host header, so every URL is portless — `http://piso.local` is the
// dashboard, `http://<label>.piso.local` is that route's worker server. The
// apex carries the dashboard UI + control API; any *.piso.local subdomain (or
// an apex carrying the ?route= / route-cookie) is proxied to the worker.
func (s *Server) WebHandler() http.Handler {
	mux := http.NewServeMux()
	api := http.NewServeMux()
	s.routes(api)
	mux.Handle("/api/", api)
	mux.HandleFunc("/", s.ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.denyControlPeer(r.RemoteAddr) {
			writeJSON(w, 403, map[string]string{"error": "control plane is host-only"})
			return
		}
		if mcpOAuthHost(r) {
			s.handleMcpOAuthHTTP(w, r)
			return
		}
		if githubAppHost(r) || strings.HasPrefix(r.URL.Path, "/github-app/") {
			s.handleGithubAppHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/oauth/ai-boost/") {
			s.handleBoostOAuth(w, r)
			return
		}
		if isWebRouteRequest(r) {
			s.ingress(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// IngressHandler returns the reverse-proxy mux for name.piso.local. Kept as a
// separate listener (host ingress port) for backward compatibility; WebHandler
// is the canonical entrypoint.
func (s *Server) IngressHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mcpOAuthHost(r) {
			s.handleMcpOAuthHTTP(w, r)
			return
		}
		if githubAppHost(r) || strings.HasPrefix(r.URL.Path, "/github-app/") {
			s.handleGithubAppHTTP(w, r)
			return
		}
		s.ingress(w, r)
	})
}

// ui serves the embedded dashboard and its favicon. Everything else 404s, so
// an unknown path on the apex can never fall through to the worker proxy.
func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		data, _ := uiFS.ReadFile("ui/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	case "/favicon.svg", "/favicon.ico":
		// Legacy browsers probe /favicon.ico; serve the same mark either way.
		s.favicon(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) favicon(w http.ResponseWriter, r *http.Request) {
	data, err := uiFS.ReadFile("ui/favicon.svg")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}

func (s *Server) routes(mux *http.ServeMux) {
	// health
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"ok": true, "time": time.Now().UTC(),
			"ingressPort": ingressHostPort(),
		})
	})
	s.registerTechnocore(mux)
	s.registerGithubApp(mux)
	s.registerGithubUser(mux)
	s.registerAIBoost(mux)
	s.registerTelegram(mux)
	mux.HandleFunc("GET /api/v1/connections", s.handleConnections)

	// CA cert for distribution
	mux.HandleFunc("GET /api/v1/ca.pem", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="piso-ca.crt"`)
		w.Write(s.CA.CertPEM())
	})

	// secrets (summaries only — real values never leave via GET)
	mux.HandleFunc("GET /api/v1/pi-provider", s.handleGetPiProvider)
	mux.HandleFunc("PUT /api/v1/pi-provider", s.handlePutPiProvider)
	mux.HandleFunc("POST /api/v1/pi-provider/activate", s.handleActivatePiProvider)
	mux.HandleFunc("POST /api/v1/pi-provider/delete", s.handleDeletePiProvider)
	mux.HandleFunc("GET /api/v1/pi-provider/models", s.handleGetPiProviderModels)

	mux.HandleFunc("GET /api/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		recs := s.Store.Secrets()
		out := make([]model.SecretSummary, 0, len(recs))
		for _, rc := range recs {
			out = append(out, storeRecToSummary(rc))
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /api/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name         string   `json:"name"`
			Placeholder  string   `json:"placeholder"`
			Value        string   `json:"value"`
			EnvKey       string   `json:"envKey"`
			AllowedHosts []string `json:"allowedHosts"`
			Workers      []string `json:"workers"`
			GatewayOnly  bool     `json:"gatewayOnly"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if in.Name == "" || in.Value == "" {
			writeJSON(w, 400, map[string]string{"error": "name and value required"})
			return
		}
		if !strings.HasPrefix(in.Placeholder, model.PlaceholderPrefix) {
			writeJSON(w, 400, map[string]string{"error": "placeholder must start with " + model.PlaceholderPrefix})
			return
		}
		// Auto-derive the worker env var name from the secret name when the
		// dashboard/CLI leave it blank; a secret without an envKey is invisible
		// to workers (SecretVisibleToWorker), which is why a derived default matters.
		if in.EnvKey == "" {
			in.EnvKey = store.DeriveEnvKey(in.Name)
		}
		if !store.ValidEnvKey(in.EnvKey) {
			writeJSON(w, 400, map[string]string{"error": "invalid envKey (set one explicitly or use a name that derives a key)"})
			return
		}
		// Destination binding is required: an empty host list used to mean
		// "every host", which silently turned every vault key into a universal
		// credential. A secret with no hosts is inert, so reject it up front
		// instead of storing something that can never substitute.
		if !in.GatewayOnly && len(store.CanonicalRuleHosts(in.AllowedHosts)) == 0 {
			writeJSON(w, 400, map[string]string{"error": "allowedHosts required: list the hosts this secret may be substituted on (use [\"*\"] only if you really want any host)"})
			return
		}
		rec := store.SecretRec{
			ID: "sec_" + randID(), Name: in.Name, Placeholder: in.Placeholder,
			Value: in.Value, EnvKey: in.EnvKey, AllowedHosts: in.AllowedHosts,
			Workers: in.Workers, GatewayOnly: in.GatewayOnly, CreatedAt: time.Now().UTC(),
		}
		// AllowedHosts is authoritative: seed the placeholder's substitution
		// rules and auto-replay any captures that were blocked on it, so adding
		// a secret unblocks them.
		retried, err := s.saveSecret(r.Context(), rec)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, map[string]any{"secret": storeRecToSummary(rec), "retried": retried, "warnings": secretHostWarnings(in.AllowedHosts)})
	})

	// PUT /api/v1/secrets/{id} — edit an existing secret (dashboard modal).
	// The real value is never returned over GET, so an empty value means
	// "keep the existing value". Placeholder is intentionally not editable
	// here because substitution rules reference it. Omitted worker-scope
	// fields preserve existing access restrictions.
	mux.HandleFunc("PUT /api/v1/secrets/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name         string    `json:"name"`
			Value        string    `json:"value"` // empty = keep existing
			EnvKey       string    `json:"envKey"`
			AllowedHosts []string  `json:"allowedHosts"`
			Workers      *[]string `json:"workers"`
			GatewayOnly  *bool     `json:"gatewayOnly"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		id := r.PathValue("id")
		existing, ok := s.Store.SecretByID(id)
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		rec := existing
		if in.Name != "" {
			rec.Name = in.Name
		}
		if in.Value != "" {
			rec.Value = in.Value
		}
		if in.EnvKey == "" {
			in.EnvKey = store.DeriveEnvKey(rec.Name)
		}
		if !store.ValidEnvKey(in.EnvKey) {
			writeJSON(w, 400, map[string]string{"error": "invalid envKey (set one explicitly or use a name that derives a key)"})
			return
		}
		rec.EnvKey = in.EnvKey
		// Explicit scope updates replace the previous values. Omission must
		// not broaden access for older clients editing unrelated fields.
		// Scope is applied before the host check so that a single PUT that
		// also leaves gateway-only mode is validated against the new scope.
		if in.GatewayOnly != nil {
			rec.GatewayOnly = *in.GatewayOnly
		}
		if in.Workers != nil {
			rec.Workers = *in.Workers
		}
		// AllowedHosts is always sent by the modal and replaces the previous
		// set wholesale; the placeholder's substitution rules are re-seeded to
		// match. Matching retryable captures are replayed so an edit that newly
		// allows a host also unblocks requests that were stuck on it.
		rec.AllowedHosts = in.AllowedHosts
		if !rec.GatewayOnly && len(store.CanonicalRuleHosts(rec.AllowedHosts)) == 0 {
			writeJSON(w, 400, map[string]string{"error": "allowedHosts required: list the hosts this secret may be substituted on (use [\"*\"] only if you really want any host)"})
			return
		}
		if err := s.Store.ReplaceSecret(rec); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		retried, err := s.seedSecretRules(r.Context(), rec)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"secret": storeRecToSummary(rec), "retried": retried, "warnings": secretHostWarnings(rec.AllowedHosts)})
	})
	mux.HandleFunc("DELETE /api/v1/secrets/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rec, ok := s.Store.SecretByID(id)
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		if err := s.revokeOrDelete(rec); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(204)
	})

	// rules (substitution)
	mux.HandleFunc("GET /api/v1/mcp", s.handleListMCP)
	mux.HandleFunc("GET /api/v1/mcp/pending", s.handlePendingMCP)
	mux.HandleFunc("POST /api/v1/mcp/{id}/authorize", s.handleAuthorizeMCP)
	mux.HandleFunc("POST /api/v1/mcp/{id}/revoke", s.handleRevokeMCP)

	mux.HandleFunc("GET /api/v1/rules", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.Store.Rules())
	})
	mux.HandleFunc("POST /api/v1/rules", func(w http.ResponseWriter, r *http.Request) {
		var in store.RuleRec
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if in.ID == "" {
			in.ID = "rule_" + randID()
		}
		if err := s.Store.AddRule(in); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, in)
	})
	mux.HandleFunc("DELETE /api/v1/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.DeleteRule(r.PathValue("id")); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(204)
	})

	// domains (deny list / needs-rule)
	mux.HandleFunc("GET /api/v1/domains", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.Store.Domains())
	})
	mux.HandleFunc("POST /api/v1/domains", func(w http.ResponseWriter, r *http.Request) {
		var in store.DomainRec
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if in.Host == "" {
			writeJSON(w, 400, map[string]string{"error": "host required"})
			return
		}
		if err := s.Store.UpsertDomain(in); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, in)
	})
	mux.HandleFunc("DELETE /api/v1/domains/{host}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.DeleteDomain(r.PathValue("host")); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(204)
	})

	// exceptions
	mux.HandleFunc("GET /api/v1/exceptions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.Store.Exceptions())
	})
	mux.HandleFunc("POST /api/v1/exceptions", func(w http.ResponseWriter, r *http.Request) {
		var in store.ExceptionRec
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if in.ID == "" {
			in.ID = "exc_" + randID()
		}
		if in.HostRegex == "" && in.PathRegex == "" && in.ContentRegex == "" && in.Placeholder == "" && in.PatternID == "" {
			writeJSON(w, 400, map[string]string{"error": "at least one matcher required"})
			return
		}
		if err := s.Store.AddException(in); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, in)
	})
	mux.HandleFunc("DELETE /api/v1/exceptions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.DeleteException(r.PathValue("id")); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(204)
	})

	// patterns (library — list; add/delete persists then reloads the compiled
	// library so the proxy picks it up live)
	mux.HandleFunc("GET /api/v1/patterns", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.Patterns.Entries())
	})
	mux.HandleFunc("POST /api/v1/patterns", func(w http.ResponseWriter, r *http.Request) {
		var in patterns.Entry
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if in.Regex == "" {
			writeJSON(w, 400, map[string]string{"error": "regex required"})
			return
		}
		if in.ID == "" {
			in.ID = "custom_" + randID()
		}
		in.Enabled = true
		if err := s.Store.AddOrReplacePattern(in); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if err := s.reloadPatterns(); err != nil {
			writeJSON(w, 500, map[string]string{"error": "stored but failed to reload: " + err.Error()})
			return
		}
		writeJSON(w, 201, in)
	})
	mux.HandleFunc("DELETE /api/v1/patterns/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.DeletePattern(r.PathValue("id")); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if err := s.reloadPatterns(); err != nil {
			writeJSON(w, 500, map[string]string{"error": "stored but failed to reload: " + err.Error()})
			return
		}
		w.WriteHeader(204)
	})

	// request log
	mux.HandleFunc("GET /api/v1/requests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.Store.Records(200))
	})
	mux.HandleFunc("GET /api/v1/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		rec, ok := s.Store.ReplayGet(r.PathValue("id"))
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, rec)
	})
	// retry: re-run policy fresh (never trust a stale decision), then forward.
	mux.HandleFunc("POST /api/v1/requests/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		rec, ok := s.Store.ReplayGet(r.PathValue("id"))
		if !ok || rec.Capture == nil {
			writeJSON(w, 404, map[string]string{"error": "no captured request"})
			return
		}
		res := s.replayOne(r.Context(), rec)
		if res.Error != "" {
			writeJSON(w, 409, map[string]string{"error": res.Error})
			return
		}
		writeJSON(w, 200, res)
	})
	// Sequential replay of every captured block that shares a placeholder
	// (and host, unless host is "*"). Oldest first.
	mux.HandleFunc("POST /api/v1/requests/retry-matching", func(w http.ResponseWriter, r *http.Request) {
		var in RetryMatch
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if in.Placeholder == "" {
			writeJSON(w, 400, map[string]string{"error": "placeholder required"})
			return
		}
		matches := matchingRetryable(s.Store.Records(0), in.Placeholder, in.Host)
		out := make([]RetryResult, 0, len(matches))
		for _, rec := range matches {
			fresh, ok := s.Store.ReplayGet(rec.ID)
			if !ok {
				out = append(out, RetryResult{ID: rec.ID, Error: "gone"})
				continue
			}
			out = append(out, s.replayOne(r.Context(), fresh))
		}
		writeJSON(w, 200, map[string][]RetryResult{"results": out})
	})
	mux.HandleFunc("POST /api/v1/failures/resolve", s.handleResolveFailures)
	mux.HandleFunc("POST /api/v1/failures/except", s.handleExceptFailures)
	mux.HandleFunc("POST /api/v1/imports/pi-keys", s.handleImportPiKeys)

	// SSE request stream
	mux.HandleFunc("GET /api/v1/requests/stream", s.stream)

	// routes (ingress)
	mux.HandleFunc("GET /api/v1/routes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.Store.Routes())
	})
	mux.HandleFunc("POST /api/v1/routes", func(w http.ResponseWriter, r *http.Request) {
		var in store.RouteRec
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		if in.ID == "" {
			in.ID = "route_" + randID()
		}
		// Host-created routes are sticky "expose" routes (the worker watcher
		// marks its own with origin "auto" via /api/v1/worker/ports).
		if in.Origin == "" {
			in.Origin = store.AutoRouteOriginExpose
		}
		if in.Name == "" || in.Port == 0 {
			writeJSON(w, 400, map[string]string{"error": "name and port required"})
			return
		}
		if err := s.Store.AddRoute(in); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, in)
	})
	mux.HandleFunc("DELETE /api/v1/routes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.DeleteRoute(r.PathValue("id")); err != nil {
			if errors.Is(err, store.ErrRouteNotFound) {
				writeJSON(w, 404, map[string]string{"error": "not found"})
				return
			}
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(204)
	})
	// block / unblock a route (the off/on toggle — keeps the row, stops proxying)
	mux.HandleFunc("POST /api/v1/routes/{id}/disabled", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Disabled bool `json:"disabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		rec, err := s.Store.SetRouteDisabled(r.PathValue("id"), in.Disabled)
		if err != nil {
			if errors.Is(err, store.ErrRouteNotFound) {
				writeJSON(w, 404, map[string]string{"error": "not found"})
				return
			}
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, rec)
	})

	// GET /api/v1/activities — host board read (the control plane is host-only;
	// the monitor uses the worker API on :8083 instead).
	mux.HandleFunc("GET /api/v1/activities", func(w http.ResponseWriter, r *http.Request) {
		f := store.ActivityFilter{
			Kind:       strings.TrimSpace(r.URL.Query().Get("kind")),
			Slug:       strings.TrimSpace(r.URL.Query().Get("slug")),
			TargetSlug: strings.TrimSpace(r.URL.Query().Get("target")),
			Limit:      atoiDefault(r.URL.Query().Get("limit"), 500),
		}
		// The live timeline needs a complete, time-bounded window rather than
		// a row limit that can silently discard older projects on busy boards.
		if r.URL.Query().Get("window") == "3h" {
			f.SinceMs = time.Now().Add(-3 * time.Hour).UnixMilli()
			f.Limit = 0
		}
		acts, err := s.Store.QueryActivities(f)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if acts == nil {
			acts = []store.Activity{}
		}
		writeJSON(w, 200, feedView(acts, time.Now().UnixNano()/1000000))
	})

	// worker registry (slug↔IP), populated by the host CLI at `piso up`
	mux.HandleFunc("GET /api/v1/workers", s.handleGetWorkers)
	mux.HandleFunc("POST /api/v1/workers", s.handlePostWorkers)
	mux.HandleFunc("DELETE /api/v1/workers/{name}", s.handleDeleteWorker)
	mux.HandleFunc("POST /api/v1/projects/{slug}/visibility", s.handleProjectVisibility)
	mux.HandleFunc("POST /api/v1/workers/{name}/internet", s.handleSetWorkerInternet)

	// Per-worker model overrides: a pin from a worker to one provider in the
	// shared stack. The active provider stays the account-wide default.
	mux.HandleFunc("GET /api/v1/model-overrides", s.handleGetModelOverrides)
	mux.HandleFunc("PUT /api/v1/model-overrides", s.handlePutModelOverride)
	mux.HandleFunc("DELETE /api/v1/model-overrides/{slug}", s.handleDeleteModelOverride)

	// Host inbox: list / approve / dismiss. Create and cancel are worker-only
	// on WorkerHandler (/api/v1/worker/planning).
	mux.HandleFunc("GET /api/v1/ingress/requests", s.handleListIngress)
	mux.HandleFunc("POST /api/v1/ingress/requests/{id}/approve", s.handleApproveIngress)
	mux.HandleFunc("POST /api/v1/ingress/requests/{id}/dismiss", s.handleDismissIngress)
	s.registerTasks(mux)
	s.registerDirectives(mux)
	mux.HandleFunc("GET /api/v1/asks", s.handleAsksList)
	mux.HandleFunc("POST /api/v1/asks/approvals/{id}/decide", s.handleAskApprove)
	mux.HandleFunc("POST /api/v1/asks/human-actions/{id}/resolve", s.handleAskResolve)
	mux.HandleFunc("POST /api/v1/tasks/{id}/respond", s.handleTaskRespond)
}

// reloadPatterns swaps the live compiled library in both the server and the
// proxy after a patterns-file mutation.
func (s *Server) reloadPatterns() error {
	pat, err := s.Store.LoadPatterns()
	if err != nil {
		return err
	}
	s.Patterns = pat
	s.Proxy.Patterns = pat
	return nil
}

// stream pushes new log records to the browser/CLI over SSE.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	for _, rec := range s.Store.Records(25) {
		if b, err := json.Marshal(rec); err == nil {
			fmt.Fprintf(w, "event: record\ndata: %s\n\n", b)
		}
	}
	fl.Flush()

	sub := s.Store.Sub()
	subIng := s.Store.SubIngress()
	subRoutes := s.Store.SubRoutes()
	subActs := s.Store.SubActivities()
	subMcp := s.Store.SubMcp()
	subGithub := s.Store.SubGithub()
	subAsks := s.Store.SubAsks()
	ctx := r.Context()
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case rec := <-sub:
			if b, err := json.Marshal(rec); err == nil {
				fmt.Fprintf(w, "event: record\ndata: %s\n\n", b)
				fl.Flush()
			}
		case irec := <-subIng:
			if b, err := json.Marshal(viewIngress(irec, true)); err == nil {
				fmt.Fprintf(w, "event: planning\ndata: %s\n\n", b)
				fl.Flush()
			}
		case rte := <-subRoutes:
			if b, err := json.Marshal(rte); err == nil {
				fmt.Fprintf(w, "event: route\ndata: %s\n\n", b)
				fl.Flush()
			}
		case act := <-subActs:
			if b, err := json.Marshal(act); err == nil {
				fmt.Fprintf(w, "event: activity\ndata: %s\n\n", b)
				fl.Flush()
			}
		case mcp := <-subMcp:
			if b, err := json.Marshal(mcp.ToSummary()); err == nil {
				fmt.Fprintf(w, "event: mcp\ndata: %s\n\n", b)
				fl.Flush()
			}
		case gha := <-subGithub:
			if b, err := json.Marshal(gha.ToSummary()); err == nil {
				fmt.Fprintf(w, "event: github\ndata: %s\n\n", b)
				fl.Flush()
			}
		case <-subAsks:
			fmt.Fprintf(w, "event: asks\ndata: {}\n\n")
			fl.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// ingress proxies a route to worker:port. The browser can reach it as
// name.piso.local (needs a hosts line) or as piso.local?route=name (apex
// already resolves). A cookie keeps the route on later same-origin fetches.
func (s *Server) ingress(w http.ResponseWriter, r *http.Request) {
	if q := strings.TrimSpace(r.URL.Query().Get(ingressRouteQuery)); validIngressLabel(q) {
		// Canonical form is the portless subdomain: piso.local?route=x becomes
		// http://x.piso.local[<webport>]<path>. Keeping the browser on the
		// apex with a route cookie is what let a stale cookie hijack the whole
		// dashboard — so route any explicit ?route= to the subdomain instead.
		next := *r.URL
		qs := next.Query()
		qs.Del(ingressRouteQuery)
		next.RawQuery = qs.Encode()
		port := ingressHostPort()
		host := q + ".piso.local"
		dest := host + next.RequestURI()
		if port != 80 {
			dest = host + fmt.Sprintf(":%d", port) + next.RequestURI()
		}
		http.Redirect(w, r, "http://"+dest, http.StatusFound)
		return
	}
	name := ingressRouteName(r)
	route, ok := s.Store.RouteByName(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if route.Disabled {
		// Blocked by the off toggle: refuse to proxy. 403 keeps it distinct
		// from an unknown label (404).
		http.Error(w, "route disabled", http.StatusForbidden)
		return
	}
	target, err := url.Parse(fmt.Sprintf("http://%s:%d", route.Worker, route.Port))
	if err != nil {
		http.Error(w, "bad route", http.StatusInternalServerError)
		return
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1 // flush immediately (SSE-friendly)
	log.Printf("ingress: %s → %s:%d", name, route.Worker, route.Port)
	rp.ServeHTTP(w, r)
}

// replayOne re-runs policy on a captured row, updates the in-memory log, and
// returns the upstream status (or a still-blocked error).
func (s *Server) replayOne(ctx context.Context, rec store.Record) RetryResult {
	out, dec, err := s.Proxy.Replay(ctx, rec)
	if err != nil {
		return RetryResult{ID: rec.ID, Error: err.Error()}
	}
	defer out.Body.Close()
	io.Copy(io.Discard, out.Body)
	rec.Action = string(dec.Action)
	rec.Reasons = reasonStrings(dec.Reasons)
	rec.Status = out.StatusCode
	rec.Retryable = false
	s.Store.ReplayUpdate(rec)
	return RetryResult{ID: rec.ID, Status: out.StatusCode}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func storeRecToSummary(rc store.SecretRec) model.SecretSummary {
	return model.SecretSummary{
		ID: rc.ID, Name: rc.Name, Placeholder: rc.Placeholder, Kind: rc.Kind, EnvKey: rc.EnvKey,
		AllowedHosts: rc.AllowedHosts, Workers: rc.Workers, GatewayOnly: rc.GatewayOnly, CreatedAt: rc.CreatedAt,
		TechnocoreID: rc.TechnocoreID,
	}
}

func randID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
