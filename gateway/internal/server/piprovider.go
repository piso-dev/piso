package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
	"piso/gateway/internal/technocore"
	"piso/internal/piprofile"
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
	SecretID     string `json:"secretId,omitempty"`
	BaseURL      string `json:"baseUrl,omitempty"`
	Placeholder  string `json:"placeholder,omitempty"`
	DefaultModel string `json:"defaultModel"`
	Applied      bool   `json:"applied"`
	Warning      string `json:"warning,omitempty"`
	UpdatedAt    string `json:"updatedAt,omitempty"`
}

func (s *Server) handleGetPiProvider(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.piProviderView())
}

func (s *Server) handlePutPiProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SecretID     string `json:"secretId"`
		BaseURL      string `json:"baseUrl"`
		DefaultModel string `json:"defaultModel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	secretID := strings.TrimSpace(in.SecretID)
	baseURL := strings.TrimSpace(in.BaseURL)
	defaultModel := strings.TrimSpace(in.DefaultModel)
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
	// A default is only useful if the provider actually serves it. Verify
	// against the live catalog so a typo can't shadow the first-available
	// fallback inside workers.
	if defaultModel != "" {
		ids, err := s.providerModelIDs(r.Context(), rec.Value, baseURL)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": "could not verify the default model with the provider"})
			return
		}
		if !containsString(ids, defaultModel) {
			current := ""
			if p := s.Store.PiProvider(); p != nil {
				current = p.DefaultModel
			}
			// Keeping an already-saved model is allowed: the provider may be
			// briefly unreachable or mid-rename, and blocking the save would
			// also make the stale value impossible to clear.
			if defaultModel != current {
				writeJSON(w, 400, map[string]string{"error": "default model is not in the provider catalog"})
				return
			}
		}
	}
	pointer := &store.PiProviderRec{
		SecretID:     resourceIDFor(rec, secretID),
		BaseURL:      baseURL,
		DefaultModel: defaultModel,
		UpdatedAt:    time.Now().UTC(),
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
	view := piProviderView{SecretID: p.SecretID, BaseURL: p.BaseURL, DefaultModel: p.DefaultModel}
	if !p.UpdatedAt.IsZero() {
		view.UpdatedAt = p.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if rec, ok := s.secretForPiProvider(p.SecretID); ok {
		view.Placeholder = rec.Placeholder
	}
	raw, err := os.ReadFile(filepath.Join(s.piProfileDir(), "models.json"))
	var models struct {
		Providers map[string]struct {
			APIKey  string `json:"apiKey"`
			BaseURL string `json:"baseUrl"`
		} `json:"providers"`
	}
	if err == nil && json.Unmarshal(raw, &models) == nil && view.Placeholder != "" {
		provider := models.Providers[piprofile.ProviderName]
		view.Applied = provider.APIKey == view.Placeholder && provider.BaseURL == view.BaseURL
	}
	raw, err = os.ReadFile(filepath.Join(s.piProfileDir(), "settings.json"))
	var settings struct {
		Provider string `json:"defaultProvider"`
		Model    string `json:"defaultModel"`
	}
	settingsMatch := err == nil && json.Unmarshal(raw, &settings) == nil
	if p.DefaultModel == "" {
		settingsMatch = settingsMatch && settings.Provider == "" && settings.Model == ""
	} else {
		settingsMatch = settingsMatch && settings.Provider == piprofile.ProviderName && settings.Model == p.DefaultModel
	}
	view.Applied = view.Applied && settingsMatch
	if !view.Applied {
		view.Warning = "saved provider settings do not match the worker profile; save again to apply"
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
	if err := s.writePiDefaultModel(p.DefaultModel); err != nil {
		view.Applied = false
		view.Warning = err.Error()
		return view
	}
	view.Placeholder = rec.Placeholder
	view.Applied = true
	view.Warning = ""
	return view
}

// writePiDefaultModel sets the dashboard-owned defaultModel in the shared
// profile's settings.json, preserving every other key. An empty model clears
// it so workers fall back to first-available selection.
func (s *Server) writePiDefaultModel(modelID string) error {
	return piprofile.WriteDefaultModel(s.piProfileDir(), modelID)
}

// piProviderCatalog is the model list behind the dashboard's autocomplete.
// The real key is used only for this host-only request and never returned.
type piProviderCatalog struct {
	Models []string `json:"models"`
}

func (s *Server) handleGetPiProviderModels(w http.ResponseWriter, r *http.Request) {
	p := s.Store.PiProvider()
	if p == nil || p.SecretID == "" || p.BaseURL == "" {
		writeJSON(w, 409, map[string]string{"error": "configure the provider before listing models"})
		return
	}
	rec, ok := s.secretForPiProvider(p.SecretID)
	if !ok || rec.Value == "" {
		writeJSON(w, 409, map[string]string{"error": "provider secret is not on this gateway yet"})
		return
	}
	ids, err := s.providerModelIDs(r.Context(), rec.Value, p.BaseURL)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "provider model catalog is unavailable"})
		return
	}
	writeJSON(w, 200, piProviderCatalog{Models: ids})
}

// providerModelIDs calls <baseURL>/models with the real key. Only model IDs
// come back; the response body is never logged or forwarded.
func (s *Server) providerModelIDs(ctx context.Context, apiKey, baseURL string) ([]string, error) {
	endpoint, err := url.JoinPath(strings.TrimRight(baseURL, "/"), "models")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(payload.Data))
	seen := map[string]bool{}
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
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
