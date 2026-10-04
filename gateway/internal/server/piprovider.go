package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"piso/internal/piprofile"
	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
	"piso/gateway/internal/technocore"
)

// piProfileDir is the worker-mounted profile. PISO_PI_PROFILE overrides it
// (tests). The gateway container mounts ${PISO_DATA} at DataDir (/data) and
// does not set PISO_DATA, so DataDir wins over the env fallback.
func (s *Server) piProfileDir() string {
	if v := strings.TrimSpace(os.Getenv("PISO_PI_PROFILE")); v != "" {
		return v
	}
	if s != nil && strings.TrimSpace(s.DataDir) != "" {
		return filepath.Join(s.DataDir, "pi-profile")
	}
	if v := strings.TrimSpace(os.Getenv("PISO_DATA")); v != "" {
		return filepath.Join(v, "pi-profile")
	}
	return "/opt/piso/pi-profile"
}

type piProviderView struct {
	SecretID    string `json:"secretId,omitempty"`
	BaseURL     string `json:"baseUrl,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Applied     bool   `json:"applied"`
	Warning     string `json:"warning,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
}

func (s *Server) handleGetPiProvider(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.piProviderView())
}

func (s *Server) handlePutPiProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SecretID string `json:"secretId"`
		BaseURL  string `json:"baseUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	secretID := strings.TrimSpace(in.SecretID)
	baseURL := strings.TrimSpace(in.BaseURL)
	if secretID == "" && baseURL == "" {
		if err := s.Store.SetPiProvider(nil); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, s.piProviderView())
		return
	}
	if err := validatePiBaseURL(baseURL); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	rec, ok := s.secretForPiProvider(secretID)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "secret not found"})
		return
	}
	if !strings.HasPrefix(rec.Placeholder, model.PlaceholderPrefix) {
		writeJSON(w, 400, map[string]string{"error": "secret has no piso_ placeholder"})
		return
	}
	pointer := &store.PiProviderRec{
		SecretID:  resourceIDFor(rec, secretID),
		BaseURL:   baseURL,
		UpdatedAt: time.Now().UTC(),
	}
	if err := s.Store.SetPiProvider(pointer); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// Best-effort publish. Reconcile retries if the coordinator is down or
	// this gateway is not enrolled yet.
	if err := technocore.PublishPiProvider(s.Store, s.DataDir); err != nil {
		view := s.applyPiProvider()
		if view.Warning == "" {
			view.Warning = "saved locally; technocore publish: " + err.Error()
		}
		writeJSON(w, 200, view)
		return
	}
	writeJSON(w, 200, s.applyPiProvider())
}

func (s *Server) piProviderView() piProviderView {
	p := s.Store.PiProvider()
	if p == nil {
		return piProviderView{}
	}
	view := piProviderView{SecretID: p.SecretID, BaseURL: p.BaseURL}
	if !p.UpdatedAt.IsZero() {
		view.UpdatedAt = p.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if rec, ok := s.secretForPiProvider(p.SecretID); ok {
		view.Placeholder = rec.Placeholder
	}
	raw, err := os.ReadFile(filepath.Join(s.piProfileDir(), "models.json"))
	if err == nil && strings.Contains(string(raw), view.Placeholder) && strings.Contains(string(raw), view.BaseURL) {
		view.Applied = true
	}
	return view
}

// applyPiProvider resolves the stored pointer to this gateway's placeholder
// and writes models.json. A missing secret is not an error: the pointer can
// arrive before the matching envelope.
func (s *Server) applyPiProvider() piProviderView {
	view := s.piProviderView()
	p := s.Store.PiProvider()
	if p == nil || p.SecretID == "" || p.BaseURL == "" {
		return view
	}
	rec, ok := s.secretForPiProvider(p.SecretID)
	if !ok || !strings.HasPrefix(rec.Placeholder, model.PlaceholderPrefix) {
		view.Applied = false
		view.Warning = "selected secret is not on this gateway yet"
		return view
	}
	if err := piprofile.WriteProviderModels(s.piProfileDir(), rec.Placeholder, p.BaseURL); err != nil {
		view.Applied = false
		view.Warning = err.Error()
		return view
	}
	view.Placeholder = rec.Placeholder
	view.Applied = true
	view.Warning = ""
	return view
}

func (s *Server) secretForPiProvider(id string) (store.SecretRec, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return store.SecretRec{}, false
	}
	if rec, ok := s.Store.SecretByTechnocoreID(id); ok {
		return rec, true
	}
	if rec, ok := s.Store.SecretByID(id); ok {
		return rec, true
	}
	if rec, ok := s.Store.SecretByID("tc_" + id); ok {
		return rec, true
	}
	return store.SecretRec{}, false
}

// resourceIDFor is the id other gateways can resolve. A local-only secret
// has no technocore UUID yet, so the local id is stored until sync stamps one.
func resourceIDFor(rec store.SecretRec, requested string) string {
	if rec.TechnocoreID != "" {
		return rec.TechnocoreID
	}
	if strings.HasPrefix(rec.ID, "tc_") {
		return strings.TrimPrefix(rec.ID, "tc_")
	}
	if requested != "" && requested != rec.ID {
		return requested
	}
	return rec.ID
}

func validatePiBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("baseUrl must be an http(s) URL")
	}
	if u.User != nil {
		return errors.New("baseUrl must not contain credentials")
	}
	return nil
}
