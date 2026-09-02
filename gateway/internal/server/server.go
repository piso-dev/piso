package server

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"piso/gateway/internal/model"
	"piso/gateway/internal/patterns"
	"piso/gateway/internal/proxy"
	"piso/gateway/internal/store"
)

//go:embed ui/index.html
var uiFS embed.FS

// Server bundles the control plane + ingress into http.Handlers.
type Server struct {
	Store    *store.Store
	Patterns *patterns.Compiled
	Proxy    *proxy.Handler
	CA       *proxy.CA
}

// New assembles the server.
func New(st *store.Store, pat *patterns.Compiled, pr *proxy.Handler, ca *proxy.CA) *Server {
	return &Server{Store: st, Patterns: pat, Proxy: pr, CA: ca}
}

// ControlHandler returns the control-plane mux (UI + API).
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	api := http.NewServeMux()
	s.routes(api)
	mux.Handle("/api/", api)
	mux.HandleFunc("/", s.ui)
	return mux
}

// IngressHandler returns the reverse-proxy mux for name.piso.local.
func (s *Server) IngressHandler() http.Handler {
	return http.HandlerFunc(s.ingress)
}

// ui serves the embedded dashboard.
func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, _ := uiFS.ReadFile("ui/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) routes(mux *http.ServeMux) {
	// health
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "time": time.Now().UTC()})
	})

	// CA cert for distribution
	mux.HandleFunc("GET /api/v1/ca.pem", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="piso-ca.crt"`)
		w.Write(s.CA.CertPEM())
	})

	// secrets (summaries only — real values never leave via GET)
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
			AllowedHosts []string `json:"allowedHosts"`
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
		rec := store.SecretRec{
			ID: "sec_" + randID(), Name: in.Name, Placeholder: in.Placeholder,
			Value: in.Value, AllowedHosts: in.AllowedHosts, CreatedAt: time.Now().UTC(),
		}
		if err := s.Store.AddSecret(rec); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, storeRecToSummary(rec))
	})
	mux.HandleFunc("DELETE /api/v1/secrets/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.DeleteSecret(r.PathValue("id")); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(204)
	})

	// rules (substitution)
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
		if in.HostRegex == "" && in.PathRegex == "" && in.ContentRegex == "" && in.Placeholder == "" {
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
		out, err := s.Proxy.Replay(r.Context(), rec)
		if err != nil {
			writeJSON(w, 409, map[string]string{"error": err.Error()})
			return
		}
		defer out.Body.Close()
		io.Copy(io.Discard, out.Body)
		writeJSON(w, 200, map[string]any{"status": out.StatusCode})
	})

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
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(204)
	})
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
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// ingress proxies name.piso.local → worker:port, supporting websocket upgrade
// (httputil.ReverseProxy handles Upgrade natively).
func (s *Server) ingress(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	name := host
	if i := strings.Index(host, "."); i >= 0 {
		name = host[:i]
	}
	route, ok := s.Store.RouteByName(name)
	if !ok {
		http.NotFound(w, r)
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func storeRecToSummary(rc store.SecretRec) model.SecretSummary {
	return model.SecretSummary{
		ID: rc.ID, Name: rc.Name, Placeholder: rc.Placeholder,
		AllowedHosts: rc.AllowedHosts, CreatedAt: rc.CreatedAt,
	}
}

func randID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}