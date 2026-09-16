package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
)

const mcpOAuthLabel = "mcp-oauth"

type mcpAnnounceIn struct {
	Worker string `json:"worker"`
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	URL    string `json:"url"`
}

func (s *Server) handleWorkerPostMCP(w http.ResponseWriter, r *http.Request) {
	var in mcpAnnounceIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Worker = strings.TrimSpace(in.Worker)
	in.Slug = strings.TrimSpace(in.Slug)
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	if !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "worker name required"})
		return
	}
	if in.Slug == "" {
		in.Slug = slugFromWorker(in.Worker)
	}
	if !workerNameRe.MatchString(in.Slug) {
		writeJSON(w, 400, map[string]string{"error": "slug required"})
		return
	}
	if reg, ok := s.Store.WorkerByIP(requestIP(r)); ok && reg.Name != in.Worker {
		identityMismatch(w, r, reg, in.Worker, in.Slug)
		return
	}
	host, resource, err := validateMCPURL(in.URL)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if in.Name == "" {
		in.Name = host
	}
	name := sanitizeMCPName(in.Name)
	rec, created, err := s.upsertMCP(in.Worker, in.Slug, name, in.URL, host, resource)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	status := 200
	if created {
		status = 201
	}
	writeJSON(w, status, map[string]any{
		"name":        rec.Name,
		"placeholder": rec.Placeholder,
		"url":         rec.URL,
		"status":      rec.Status,
		"mcp":         rec.ToSummary(),
	})
}

func (s *Server) upsertMCP(worker, slug, name, rawURL, host, resource string) (store.McpServerRec, bool, error) {
	if existing, ok := s.Store.FindMcp(worker, name, rawURL); ok && existing.Placeholder != "" {
		return existing, false, nil
	}
	now := time.Now().UTC()
	ph := s.mintMCPPlaceholder(slug, name)
	sec := store.SecretRec{
		ID:           "sec_" + randID(),
		Name:         "mcp:" + slug + ":" + name,
		Placeholder:  ph,
		Value:        "",
		EnvKey:       store.DeriveEnvKey("MCP_" + slug + "_" + name),
		AllowedHosts: []string{host},
		Workers:      []string{slug},
		CreatedAt:    now,
	}
	if err := s.Store.AddSecret(sec); err != nil {
		return store.McpServerRec{}, false, err
	}
	if err := s.Store.SyncRulesForSecret(sec); err != nil {
		return store.McpServerRec{}, false, err
	}
	rec := store.McpServerRec{
		ID:          "mcp_" + randID(),
		Worker:      worker,
		Slug:        slug,
		Name:        name,
		URL:         rawURL,
		Host:        host,
		Placeholder: ph,
		SecretID:    sec.ID,
		Status:      store.McpStatusNeedsAuth,
		Resource:    resource,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	out, created, err := s.Store.UpsertMcpServer(rec)
	if err != nil {
		return store.McpServerRec{}, false, err
	}
	return out, created, nil
}

func (s *Server) mintMCPPlaceholder(slug, name string) string {
	base := "piso_mcp_" + slug + "_" + name
	base = strings.ReplaceAll(base, ".", "-")
	if !s.Store.PlaceholderTaken(base) {
		return base
	}
	for i := 0; i < 8; i++ {
		cand := base + "_" + randID()
		if !s.Store.PlaceholderTaken(cand) {
			return cand
		}
	}
	return base + "_" + fmt.Sprintf("%d", time.Now().UnixNano())
}

func (s *Server) handleListMCP(w http.ResponseWriter, r *http.Request) {
	recs := s.Store.McpServers()
	out := make([]model.McpServer, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.ToSummary())
	}
	writeJSON(w, 200, out)
}

func (s *Server) handlePendingMCP(w http.ResponseWriter, r *http.Request) {
	recs := s.Store.PendingMcpAuth()
	out := make([]model.McpServer, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.ToSummary())
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleAuthorizeMCP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, ok := s.Store.McpByID(id)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	authURL, err := s.startMCPOAuth(r.Context(), rec)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"url": authURL, "id": rec.ID})
}

func (s *Server) handleRevokeMCP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, ok := s.Store.McpByID(id)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	if sec, ok := s.Store.SecretByID(rec.SecretID); ok {
		sec.Value = ""
		if err := s.Store.ReplaceSecret(sec); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	}
	rec.RefreshToken = ""
	rec.CodeVerifier = ""
	rec.OAuthState = ""
	rec.ExpiresAt = nil
	rec.Status = store.McpStatusRevoked
	if err := s.Store.ReplaceMcpServer(rec); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, rec.ToSummary())
}

func validateMCPURL(raw string) (host, resource string, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("url must be an absolute URL")
	}
	if u.Scheme != "https" {
		return "", "", fmt.Errorf("url must use HTTPS")
	}
	if u.User != nil {
		return "", "", fmt.Errorf("url must not contain credentials")
	}
	if u.Fragment != "" {
		return "", "", fmt.Errorf("url must not contain a fragment")
	}
	h := strings.ToLower(u.Hostname())
	if h == "" {
		return "", "", fmt.Errorf("url host required")
	}
	if ip := net.ParseIP(h); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return "", "", fmt.Errorf("url host must not be internal")
		}
	}
	switch h {
	case "localhost", "gateway", "piso.local":
		return "", "", fmt.Errorf("url host must not be internal")
	}
	if strings.HasSuffix(h, ".piso.local") {
		return "", "", fmt.Errorf("url host must not be internal")
	}
	u.Host = h
	if u.Port() != "" && u.Port() != "443" {
		u.Host = net.JoinHostPort(h, u.Port())
	}
	return h, strings.TrimRight(u.String(), "/"), nil
}

func sanitizeMCPName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := true
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "mcp"
	}
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-")
	}
	return out
}

func mcpOAuthHost(r *http.Request) bool {
	return ingressHostname(r.Host) == mcpOAuthLabel+".piso.local"
}
