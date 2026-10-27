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
	ID             string   `json:"id"`
	Name           string   `json:"name,omitempty"`
	SecretID       string   `json:"secretId,omitempty"`
	BaseURL        string   `json:"baseUrl,omitempty"`
	Kind           string   `json:"kind,omitempty"` // "" remote | "tinfoil"
	Placeholder    string   `json:"placeholder,omitempty"`
	DefaultModel   string   `json:"defaultModel"`
	EffectiveModel string   `json:"effectiveModel,omitempty"`
	API            string   `json:"api,omitempty"`
	Active         bool     `json:"active"`
	Applied        bool     `json:"applied"`
	Warning        string   `json:"warning,omitempty"`
	Catalog        string   `json:"catalog,omitempty"` // ok | failed | pending | local
	CatalogError   string   `json:"catalogError,omitempty"`
	Models         []string `json:"models,omitempty"`
	CheckedAt      string   `json:"checkedAt,omitempty"`
	UpdatedAt      string   `json:"updatedAt,omitempty"`
}

type piProviderListView struct {
	Providers []piProviderView `json:"providers"`
	ActiveID  string           `json:"activeId"`
	// DefaultProvider/DefaultModel mirror the activated profile at the top
	// level so a CLI that predates the provider stack (or an older
	// coordinator) still reads the startup default from this response.
	DefaultProvider string `json:"defaultProvider,omitempty"`
	DefaultModel    string `json:"defaultModel"`
	Applied         bool   `json:"applied"`
	Warning         string `json:"warning,omitempty"`
}

func (s *Server) handleGetPiProvider(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.piProviderListView())
}

// handlePutPiProvider adds or updates one profile. It never contacts the
// provider: a failed catalog fetch must not block saving or activating a
// provider. The dashboard fetches the catalog separately for the picker.
func (s *Server) handlePutPiProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		SecretID     string   `json:"secretId"`
		BaseURL      string   `json:"baseUrl"`
		Kind         string   `json:"kind"`
		DefaultModel string   `json:"defaultModel"`
		API          string   `json:"api"`
		Models       []string `json:"models"`
		Activate     bool     `json:"activate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	kind, baseURL, err := piProviderEndpoint(strings.TrimSpace(in.Kind), strings.TrimSpace(in.BaseURL))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := validatePiBaseURL(baseURL); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	rec, ok := s.secretForPiProvider(in.SecretID)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "secret not found"})
		return
	}
	if !strings.HasPrefix(rec.Placeholder, model.PlaceholderPrefix) {
		writeJSON(w, 400, map[string]string{"error": "secret has no piso_ placeholder"})
		return
	}
	// A provider's apiKey is substituted as a literal credential on the model
	// host. Kinds that are not literal values (github-app mints per host,
	// telegram is a bot token) or that workers may not use would only fail at
	// request time with a confusing rule or mint error, so refuse the binding
	// here, where the operator can still see why.
	if rec.Kind != model.SecretKindOpaque {
		writeJSON(w, 400, map[string]string{"error": "provider secret must be a plain API key, not a " + rec.Kind + " credential"})
		return
	}
	if rec.GatewayOnly {
		writeJSON(w, 400, map[string]string{"error": "provider secret must be usable by workers; this one is gateway-only"})
		return
	}

	id := strings.TrimSpace(in.ID)
	if id != "" && !piprofile.ManagedProviderKey(id) {
		writeJSON(w, 400, map[string]string{"error": "provider id is not in the piso namespace"})
		return
	}
	existing := (*store.PiProviderRec)(nil)
	if id != "" {
		existing = s.Store.PiProviderByID(id)
	}
	if id == "" {
		id = s.uniquePiProviderID(providerIDFrom(in.Name, baseURL, ""))
	}
	warning := ""
	pointer := &store.PiProviderRec{
		ID:           id,
		Name:         strings.TrimSpace(in.Name),
		SecretID:     resourceIDFor(rec, in.SecretID),
		BaseURL:      baseURL,
		Kind:         kind,
		DefaultModel: strings.TrimSpace(in.DefaultModel),
		APIType:      strings.TrimSpace(in.API),
		UpdatedAt:    time.Now().UTC(),
	}
	// Keep the last known catalog; it is advisory and the save path never
	// refreshes it. A different endpoint invalidates it.
	if existing != nil && existing.BaseURL == baseURL {
		pointer.Models = append([]string(nil), existing.Models...)
		pointer.CatalogCheckedAt = existing.CatalogCheckedAt
		pointer.CatalogError = existing.CatalogError
	}
	// A picker load is fresher than anything stored. Persisting it here keeps
	// the provider card showing the same list the modal just offered, which
	// matters for a brand new provider that had no catalog before.
	if loaded := cleanModelIDs(in.Models); len(loaded) > 0 {
		pointer.Models = loaded
		pointer.CatalogCheckedAt = time.Now().UTC()
		pointer.CatalogError = ""
	}
	if pointer.DefaultModel != "" && len(pointer.Models) > 0 && !containsString(pointer.Models, pointer.DefaultModel) {
		warning = "default model is not in the provider's last known catalog; it was saved anyway"
	}
	// A tinfoil provider's real request leaves the worker's proxy for a router
	// enclave, so a secret bound only to loopback substitutes nothing and every
	// call fails 407. Warn rather than refuse: the secret's hosts are its own
	// field, and a hand-built config may bind hosts this rule cannot know.
	if kind == piProviderKindTinfoil && !secretCoversTinfoilFleet(rec.AllowedHosts) {
		warning = joinWarning(warning, "the selected secret is not bound to *.tinfoil.sh; the proxy's real request leaves for a numbered router enclave, so a loopback or unrelated host rule substitutes nothing")
	}
	if in.Activate {
		err = s.Store.UpsertPiProviderActive(*pointer)
	} else {
		err = s.Store.UpsertPiProvider(*pointer)
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	publishWarning := s.publishPiProvider()
	s.applyPiProviders()
	view := s.piProviderViewByID(id)
	if view == nil {
		view = &piProviderView{ID: id}
	}
	if warning != "" {
		view.Warning = joinWarning(warning, view.Warning)
	}
	if publishWarning != "" {
		view.Warning = joinWarning(view.Warning, publishWarning)
	}
	writeJSON(w, 200, view)
}

// handleActivatePiProvider selects one profile as the startup default.
func (s *Server) handleActivatePiProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	if err := s.Store.ActivatePiProvider(strings.TrimSpace(in.ID)); err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	s.publishPiProvider()
	view := s.applyPiProviders()
	writeJSON(w, 200, view)
}

// handleDeletePiProvider removes a profile and rewrites the profile files.
func (s *Server) handleDeletePiProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	if err := s.Store.DeletePiProvider(strings.TrimSpace(in.ID)); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.publishPiProvider()
	writeJSON(w, 200, s.applyPiProviders())
}

// piProviderViewByID builds the view for one saved profile, or nil.
func (s *Server) piProviderViewByID(id string) *piProviderView {
	p := s.Store.PiProviderByID(id)
	if p == nil {
		return nil
	}
	fs := s.readProfileFiles()
	view := s.providerViewFor(p, fs)
	return &view
}

func (s *Server) piProviderListView() piProviderListView {
	profiles := s.Store.PiProviderStack()
	fs := s.readProfileFiles()
	activeID := s.Store.ActivePiProviderID()
	out := piProviderListView{Providers: make([]piProviderView, 0, len(profiles)), ActiveID: activeID}
	var active *store.PiProviderRec
	for i := range profiles {
		view := s.providerViewFor(&profiles[i], fs)
		if view.Active {
			cp := profiles[i]
			active = &cp
			out.Applied = view.Applied
			out.Warning = view.Warning
		}
		out.Providers = append(out.Providers, view)
	}
	if active == nil {
		// No active selection: the profile clears the default and pi falls
		// back to first-available. That is applied by definition.
		out.Applied = true
	}
	// effectivePiModel applies the same catalog fallback the worker resolves
	// against, so the flat fields never name a model the profile cannot use.
	if active != nil {
		out.DefaultProvider = active.ID
		out.DefaultModel = effectivePiModel(active)
	}
	return out
}

// providerViewFor resolves one profile against the profile files on disk.
func (s *Server) providerViewFor(p *store.PiProviderRec, fs profileFileState) piProviderView {
	activeID := s.Store.ActivePiProviderID()
	view := piProviderView{
		ID:             p.ID,
		Name:           p.Name,
		SecretID:       p.SecretID,
		BaseURL:        p.BaseURL,
		Kind:           p.Kind,
		DefaultModel:   p.DefaultModel,
		EffectiveModel: effectivePiModel(p),
		API:            p.APIType,
		Active:         p.ID == activeID,
		CatalogError:   p.CatalogError,
		Models:         append([]string(nil), p.Models...),
	}
	if !p.UpdatedAt.IsZero() {
		view.UpdatedAt = p.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if !p.CatalogCheckedAt.IsZero() {
		view.CheckedAt = p.CatalogCheckedAt.UTC().Format(time.RFC3339)
	}
	switch {
	case p.CatalogError != "":
		view.Catalog = "failed"
	case len(p.Models) > 0 || !p.CatalogCheckedAt.IsZero():
		// A catalog that loaded but listed no models is known, not pending.
		view.Catalog = "ok"
	default:
		view.Catalog = "pending"
	}
	// A local provider's catalog lives inside the worker, behind a proxy the
	// gateway has no route to. Reporting "pending" would say the gateway is
	// still asking, which it never will: the list is only ever as fresh as the
	// last save, so name where it comes from instead.
	if piProviderIsLocal(p) {
		view.Catalog = "local"
	}
	rec, ok := s.secretForPiProvider(p.SecretID)
	if !ok || !strings.HasPrefix(rec.Placeholder, model.PlaceholderPrefix) {
		// The secret is not resolvable on this gateway right now. If the shared
		// profile already carries a working placeholder for this provider, it
		// is not unconfigured: it is applied and serving traffic while the
		// secret syncs. Say that instead of warning about a missing secret.
		if entry, present := fs.providers[p.ID]; present && strings.HasPrefix(entry.APIKey, model.PlaceholderPrefix) && entry.BaseURL == p.BaseURL {
			view.Placeholder = entry.APIKey
			view.Applied = true
			return view
		}
		view.Warning = "selected secret is not on this gateway yet"
		return view
	}
	view.Placeholder = rec.Placeholder
	entry, present := fs.providers[p.ID]
	view.Applied = present && entry.APIKey == rec.Placeholder && entry.BaseURL == p.BaseURL
	if !view.Applied {
		view.Warning = joinWarning(view.Warning, "saved provider settings do not match the worker profile; save again to apply")
		return view
	}
	if view.Active && !fs.settingsMatch {
		view.Applied = false
		view.Warning = joinWarning(view.Warning, "worker default model does not match the active provider; save again to apply")
	}
	return view
}

// profileFileState is the dashboard-owned slice of the shared profile.
type profileFileState struct {
	providers     map[string]struct{ APIKey, BaseURL string }
	settingsMatch bool
}

func (s *Server) readProfileFiles() profileFileState {
	fs := profileFileState{providers: map[string]struct{ APIKey, BaseURL string }{}}
	raw, err := os.ReadFile(filepath.Join(s.piProfileDir(), "models.json"))
	var models struct {
		Providers map[string]struct {
			APIKey  string `json:"apiKey"`
			BaseURL string `json:"baseUrl"`
		} `json:"providers"`
	}
	if err == nil && json.Unmarshal(raw, &models) == nil {
		for id, entry := range models.Providers {
			if piprofile.ManagedProviderKey(id) {
				fs.providers[id] = struct{ APIKey, BaseURL string }{entry.APIKey, entry.BaseURL}
			}
		}
	}
	active := s.Store.PiProvider()
	settingsDefaultProvider, settingsDefaultModel, _ := s.readSettingsDefault()
	if active == nil {
		fs.settingsMatch = settingsDefaultProvider == "" && settingsDefaultModel == ""
		return fs
	}
	want := effectivePiModel(active)
	if want == "" {
		fs.settingsMatch = settingsDefaultProvider == "" && settingsDefaultModel == ""
	} else {
		fs.settingsMatch = settingsDefaultProvider == active.ID && settingsDefaultModel == want
	}
	return fs
}

// readSettingsDefault returns defaultProvider/defaultModel. settingsOK is
// true when the file was readable and parsed.
func (s *Server) readSettingsDefault() (provider, model string, ok bool) {
	raw, err := os.ReadFile(filepath.Join(s.piProfileDir(), "settings.json"))
	if err != nil {
		return "", "", false
	}
	var settings struct {
		Provider string `json:"defaultProvider"`
		Model    string `json:"defaultModel"`
	}
	if json.Unmarshal(raw, &settings) != nil {
		return "", "", false
	}
	return settings.Provider, settings.Model, true
}

// applyPiProviders rewrites models.json and the active default from the
// stored stack. A profile whose secret is not on this gateway yet is skipped
// with a warning rather than failing the write.
func (s *Server) applyPiProviders() piProviderListView {
	profiles := s.Store.PiProviderStack()
	applied := s.readProfileFiles()
	providers := make([]piprofile.Provider, 0, len(profiles))
	warnings := []string{}
	for i := range profiles {
		p := &profiles[i]
		rec, ok := s.secretForPiProvider(p.SecretID)
		if !ok || !strings.HasPrefix(rec.Placeholder, model.PlaceholderPrefix) {
			// Keep a provider that is already applied: models.json still
			// carries a working placeholder, and dropping it here would break a
			// provider that is serving traffic while its secret syncs.
			if entry, present := applied.providers[p.ID]; present && strings.HasPrefix(entry.APIKey, model.PlaceholderPrefix) && entry.BaseURL == p.BaseURL {
				providers = append(providers, piprofile.Provider{ID: p.ID, Name: p.Name, APIKey: entry.APIKey, BaseURL: p.BaseURL, API: p.APIType, Models: piProfileModels(p)})
				continue
			}
			warnings = append(warnings, fmt.Sprintf("%s: selected secret is not on this gateway yet", piProviderLabel(p)))
			continue
		}
		providers = append(providers, piprofile.Provider{
			ID:      p.ID,
			Name:    p.Name,
			APIKey:  rec.Placeholder,
			BaseURL: p.BaseURL,
			API:     p.APIType,
			Models:  piProfileModels(p),
		})
	}
	if err := piprofile.WriteProviderStack(s.piProfileDir(), providers); err != nil {
		view := s.piProviderListView()
		view.Applied = false
		view.Warning = err.Error()
		return view
	}
	active := s.Store.PiProvider()
	providerID, modelID := "", ""
	if active != nil {
		// Point the default only at a provider that actually made it into
		// models.json; otherwise pi would chase a missing provider.
		for _, written := range providers {
			if written.ID == active.ID {
				providerID = active.ID
				modelID = effectivePiModel(active)
				break
			}
		}
	}
	if err := piprofile.WriteProviderDefault(s.piProfileDir(), providerID, modelID); err != nil {
		view := s.piProviderListView()
		view.Applied = false
		view.Warning = err.Error()
		return view
	}
	// The shared profile is bind-mounted read-only into every worker. Pi
	// sessions re-read it; nothing else to push here.
	view := s.piProviderListView()
	if len(warnings) > 0 {
		view.Warning = joinWarning(view.Warning, strings.Join(warnings, "; "))
	}
	return view
}

// workerPiProfileView is the profile as a worker consumes it: the two files
// verbatim, so a remote worker can materialize exactly what a colocated one
// finds in its bind mount. Every apiKey in models.json is a piso_ placeholder.
type workerPiProfileView struct {
	Settings        string `json:"settings"`
	Models          string `json:"models"`
	DefaultProvider string `json:"defaultProvider,omitempty"`
	DefaultModel    string `json:"defaultModel,omitempty"`
	Applied         bool   `json:"applied"`
	Warning         string `json:"warning,omitempty"`
}

// handleWorkerGetPiProfile serves the dashboard-managed pi profile to a worker
// that cannot read it off this host's disk.
//
// Locally the gateway rewrites $PISO_DATA/pi-profile in place and every worker
// mounts that directory read-only, so nothing has to be pushed. A cloud ship is
// a different machine: its mount is an empty directory it created itself, and
// without this endpoint pi would start there with no provider and no default
// model. The answer is the mounted file content, not a second rendering of the
// store, so a ship cannot drift from a colocated worker.
//
// A read is the normal path: the dashboard and the technocore reconcile already
// wrote the profile when something changed, and ships poll this endpoint every
// minute. Only a profile that was never written is reconciled here.
func (s *Server) handleWorkerGetPiProfile(w http.ResponseWriter, r *http.Request) {
	if worker := strings.TrimSpace(r.URL.Query().Get("worker")); worker != "" {
		if !workerNameRe.MatchString(worker) {
			writeJSON(w, 400, map[string]string{"error": "invalid worker"})
			return
		}
		if !s.workerIdentityOK(w, r, worker) {
			return
		}
	}
	settings, models, err := s.readPiProfileFiles()
	if err != nil {
		// No profile yet (a gateway that has never applied one): build it once
		// from the store, then serve the result.
		s.applyPiProviders()
		if settings, models, err = s.readPiProfileFiles(); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	}
	view := s.piProviderListView()
	writeJSON(w, 200, workerPiProfileView{
		Settings:        string(settings),
		Models:          string(models),
		DefaultProvider: view.DefaultProvider,
		DefaultModel:    view.DefaultModel,
		Applied:         view.Applied,
		Warning:         view.Warning,
	})
}

// readPiProfileFiles returns both profile files as a worker mounts them.
func (s *Server) readPiProfileFiles() (settings, models []byte, err error) {
	dir := s.piProfileDir()
	if settings, err = os.ReadFile(filepath.Join(dir, "settings.json")); err != nil {
		return nil, nil, fmt.Errorf("profile settings: %w", err)
	}
	if models, err = os.ReadFile(filepath.Join(dir, "models.json")); err != nil {
		return nil, nil, fmt.Errorf("profile models: %w", err)
	}
	return settings, models, nil
}

// publishPiProvider pushes the active pointer to any enrolled coordinator and
// returns a non-fatal warning when the publish did not go through.
func (s *Server) publishPiProvider() string {
	if err := technocore.PublishPiProvider(s.Store, s.DataDir); err != nil {
		return "saved locally; technocore publish: " + err.Error()
	}
	return ""
}

// effectivePiModel is the model new sessions should start on. An empty
// explicit default falls back to the last known catalog's first id, which is
// empty when the catalog has never been fetched.
func piProfileModels(p *store.PiProviderRec) []string {
	models := append([]string(nil), p.Models...)
	// Manual defaults must be resolvable by pi even without a provider catalog.
	if id := strings.TrimSpace(p.DefaultModel); id != "" && !containsString(models, id) {
		models = append(models, id)
	}
	return models
}

func effectivePiModel(p *store.PiProviderRec) string {
	if p == nil {
		return ""
	}
	if m := strings.TrimSpace(p.DefaultModel); m != "" {
		return m
	}
	for _, m := range p.Models {
		if m = strings.TrimSpace(m); m != "" {
			return m
		}
	}
	return ""
}

func piProviderLabel(p *store.PiProviderRec) string {
	if strings.TrimSpace(p.Name) != "" {
		return strings.TrimSpace(p.Name)
	}
	return p.ID
}

func joinWarning(a, b string) string {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}

// providerIDFrom derives a stable, namespaced profile id. A supplied id wins.
func providerIDFrom(name, baseURL, id string) string {
	if id = strings.TrimSpace(id); id != "" {
		return id
	}
	base := strings.TrimSpace(name)
	if base == "" {
		if u, err := url.Parse(baseURL); err == nil {
			base = u.Hostname()
		}
	}
	slug := slugify(base)
	if slug == "" {
		slug = "provider"
	}
	return piprofile.ProviderName + "-" + slug
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// uniquePiProviderID appends a numeric suffix until the id is free.
func (s *Server) uniquePiProviderID(base string) string {
	if s.Store.PiProviderByID(base) == nil {
		return base
	}
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if s.Store.PiProviderByID(candidate) == nil {
			return candidate
		}
	}
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
}

// piProviderCatalog is the model list behind the dashboard's autocomplete.
// The real key is used only for this host-only request and never returned. A
// failed fetch is a 200 with the last known list and catalog:"failed", never
// a hard error: the picker stays usable and the profile still saves.
type piProviderCatalog struct {
	Models    []string `json:"models"`
	Catalog   string   `json:"catalog"`
	Error     string   `json:"error,omitempty"`
	CheckedAt string   `json:"checkedAt,omitempty"`
}

func (s *Server) handleGetPiProviderModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := strings.TrimSpace(q.Get("id"))
	baseURL := strings.TrimSpace(q.Get("baseUrl"))
	secretID := strings.TrimSpace(q.Get("secretId"))
	// Resolve the saved profile when the form did not supply its own values.
	// The add dialog has no id yet, so it types baseUrl/secretId directly;
	// the edit dialog sends them so an unsaved change loads immediately.
	var saved *store.PiProviderRec
	if id != "" {
		saved = s.Store.PiProviderByID(id)
	}
	if saved == nil && baseURL == "" && secretID == "" {
		saved = s.Store.PiProvider()
	}
	if baseURL == "" && saved != nil {
		baseURL = saved.BaseURL
	}
	// Only a saved record can say whether the catalog is local. The add dialog
	// never asks for a local one: it knows the catalog is unreachable and
	// renders the explanation itself, so an unsaved request lands here as
	// remote and simply fails to fetch.
	if secretID == "" && saved != nil {
		secretID = saved.SecretID
	}
	if baseURL == "" || secretID == "" {
		writeJSON(w, 409, map[string]string{"error": "configure the provider before listing models"})
		return
	}
	if err := validatePiBaseURL(baseURL); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	// A local provider's catalog is served by a proxy that runs beside pi, in
	// the worker, so this gateway cannot reach it by design. Fetching would
	// only produce "connection refused" and a scary "catalog unavailable"
	// badge for a provider that works: pi discovers /v1/models through its own
	// proxy. Report the reason instead of failing, and leave the cached list
	// untouched. piProviderIsLocal also covers an older row saved before kind
	// existed, which would otherwise look remote and fail here.
	if piProviderIsLocal(saved) {
		out := piProviderCatalog{Catalog: "local"}
		if saved != nil && saved.BaseURL == baseURL {
			out.Models = append([]string(nil), saved.Models...)
			if !saved.CatalogCheckedAt.IsZero() {
				out.CheckedAt = saved.CatalogCheckedAt.UTC().Format(time.RFC3339)
			}
		}
		writeJSON(w, 200, out)
		return
	}
	rec, ok := s.secretForPiProvider(secretID)
	if !ok || rec.Value == "" {
		writeJSON(w, 409, map[string]string{"error": "provider secret is not on this gateway yet"})
		return
	}
	// Cached models only describe the same endpoint; a changed URL must not
	// fall back to another provider's catalog.
	cached := saved != nil && saved.BaseURL == baseURL
	ids, err := s.providerModelIDs(r.Context(), rec.Value, baseURL)
	if err != nil {
		// Record the failure for the saved endpoint but keep serving its
		// cached list so the user can still pick a model. A changed URL has no
		// cache to fall back to.
		if saved != nil && cached {
			_ = s.Store.SetPiProviderCatalog(saved.ID, nil, "provider model catalog is unavailable")
		}
		out := piProviderCatalog{Catalog: "failed", Error: "provider model catalog is unavailable"}
		if cached {
			out.Models = append([]string(nil), saved.Models...)
			if !saved.CatalogCheckedAt.IsZero() {
				out.CheckedAt = saved.CatalogCheckedAt.UTC().Format(time.RFC3339)
			}
		}
		writeJSON(w, 200, out)
		return
	}
	// Only cache a catalog that describes the saved endpoint; an unsaved
	// URL change must not overwrite the stored provider's model list.
	if saved != nil && cached {
		_ = s.Store.SetPiProviderCatalog(saved.ID, ids, "")
	}
	out := piProviderCatalog{Models: ids, Catalog: "ok", CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	writeJSON(w, 200, out)
}

// Pi provider transport kinds. A provider's kind is decided once, when the
// dashboard saves it, and stored on the record. It is never inferred from
// BaseURL at read time: the answer decides whether the gateway may fetch a
// catalog, and that must not hinge on comparing a string to a constant that
// lives in a different file.
const (
	piProviderKindRemote  = ""
	piProviderKindTinfoil = "tinfoil"
)

// piProviderIsLocal reports whether a profile's catalog is served by a proxy
// inside the worker rather than by this gateway. Kind is authoritative once
// set; an older row saved before kind existed is recognized by the exact proxy
// URL so it is not left stuck on "model list pending".
func piProviderIsLocal(p *store.PiProviderRec) bool {
	if p == nil {
		return false
	}
	return p.Kind == piProviderKindTinfoil || (p.Kind == "" && p.BaseURL == tinfoilProxyBaseURL)
}

// tinfoilProxyBaseURL is the enclave proxy the worker starts on its own
// loopback. pi reaches it because it runs in that same container; this gateway
// can only ever get connection refused. Kept in step with TINFOIL_BASE_URL in
// ui/index.html and --port in worker/entrypoint.sh.
const tinfoilProxyBaseURL = "http://127.0.0.1:3301/v1"

// tinfoilEnclaveSuffix is the domain the tinfoil proxy's sealed request leaves
// for after the worker hands it over. It is the host the operator's secret has
// to be bound to, not the loopback address pi dials.
const tinfoilEnclaveSuffix = "tinfoil.sh"

// secretCoversTinfoilFleet reports whether allowedHosts can match the router
// enclave the proxy will actually reach. The fleet is numbered (router-0,
// router-1, ...), so the check probes more than one index: a rule pinned to a
// single router works until the next attestation and then silently stops
// substituting.
func secretCoversTinfoilFleet(allowedHosts []string) bool {
	hosts := store.CanonicalRuleHosts(allowedHosts)
	if len(hosts) == 0 {
		return false
	}
	for _, probe := range []string{"router-0." + tinfoilEnclaveSuffix, "router-1." + tinfoilEnclaveSuffix} {
		covered := false
		for _, rule := range hosts {
			if model.HostRuleMatch(rule, probe) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// piProviderEndpoint resolves the stored kind and base URL for a save. A local
// kind pins its URL: the dashboard hides the field for it, so the server must
// not trust whatever the client sent. Anything else is a remote endpoint and
// keeps the operator's URL.
func piProviderEndpoint(kind, baseURL string) (string, string, error) {
	if kind != piProviderKindRemote && kind != piProviderKindTinfoil {
		return "", "", fmt.Errorf("unknown provider kind %q", kind)
	}
	if kind == piProviderKindTinfoil {
		return kind, tinfoilProxyBaseURL, nil
	}
	return piProviderKindRemote, baseURL, nil
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

// cleanModelIDs normalizes catalog ids supplied by the provider picker:
// trims, drops empties, and de-dupes while preserving order.
func cleanModelIDs(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, v := range values {
		id := strings.TrimSpace(v)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
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
