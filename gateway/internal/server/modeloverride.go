package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"piso/gateway/internal/store"
	"piso/internal/piprofile"
)

// Worker model overrides.
//
// The provider stack is account-wide: every worker mounts the same
// models.json, and the active profile is the startup default for all of them.
// An override does not write a second profile — it pins one worker to one
// already-shared provider, and the worker reads its own answer from
// GET /api/v1/worker/model. Precedence is override → account default →
// first catalog entry, resolved in exactly one place (resolveWorkerModel).
//
// Because the row is keyed by slug and the worker pulls it, a change applies at
// the worker's next poll without a container restart, and it works for remote
// ("cloud-ship") workers whose /etc/piso the gateway cannot write.

// modelOverrideView is one row of the override list as the dashboard shows it.
type modelOverrideView struct {
	Slug         string `json:"slug"`
	Worker       string `json:"worker,omitempty"`
	ProviderID   string `json:"providerId"`
	ProviderName string `json:"providerName,omitempty"`
	// Model is the model that actually applies under the pinned provider.
	Model string `json:"model,omitempty"`
	// LiveModel is what the worker last reported running (advisory; it is a
	// different thing from the configured selection).
	LiveModel string `json:"liveModel,omitempty"`
	Source    string `json:"source"`
	Warning   string `json:"warning,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// modelOverrideProviderView feeds the provider dropdown. Workers is the
// provider secret's scope (empty = every worker), so the dashboard can say up
// front that a pick will not authenticate from a worker outside that list.
type modelOverrideProviderView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name,omitempty"`
	DefaultModel string   `json:"defaultModel,omitempty"`
	Workers      []string `json:"workers,omitempty"`
}

type modelOverrideListView struct {
	Overrides []modelOverrideView `json:"overrides"`
	// Account is the default every worker without an override inherits.
	Account   modelOverrideAccountView    `json:"account"`
	Providers []modelOverrideProviderView `json:"providers"`
}

type modelOverrideAccountView struct {
	ProviderID   string `json:"providerId,omitempty"`
	ProviderName string `json:"providerName,omitempty"`
	Model        string `json:"model,omitempty"`
}

// workerModelView is the worker-facing answer: "start on this provider/model".
type workerModelView struct {
	Worker   string `json:"worker,omitempty"`
	Slug     string `json:"slug,omitempty"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Source is "override", "account", or "none" when no provider is
	// configured at all. The worker uses it for diagnostics only; precedence
	// is already applied here.
	Source  string `json:"source"`
	Warning string `json:"warning,omitempty"`
}

// resolveWorkerModel applies the precedence in one place: a worker override
// wins over the account default, and an override pointing at a provider that no
// longer exists falls back to the account default with a warning rather than
// leaving the worker with nothing.
func (s *Server) resolveWorkerModel(slug string) workerModelView {
	slug = strings.TrimSpace(slug)
	out := workerModelView{Slug: slug, Source: "none"}
	if p := s.Store.PiProviderByID(s.Store.ModelOverrideProviderID(slug)); p != nil {
		out.Provider = p.ID
		out.Model = effectivePiModel(p)
		out.Source = "override"
		return out
	}
	if _, ok := s.Store.ModelOverrideBySlug(slug); ok {
		out.Warning = "the pinned provider no longer exists; this worker uses the account default"
	}
	if p := s.Store.PiProvider(); p != nil {
		out.Provider = p.ID
		out.Model = effectivePiModel(p)
		out.Source = "account"
	}
	return out
}

// providerWorkers returns the secret scope for the provider's credential: the
// slugs allowed to see its placeholder. Empty means every worker.
func (s *Server) providerWorkers(p *store.PiProviderRec) []string {
	if p == nil {
		return nil
	}
	rec, found := s.secretForPiProvider(p.SecretID)
	if !found {
		return nil
	}
	return append([]string(nil), rec.Workers...)
}

// providerReachableFrom reports whether slug can authenticate a request with
// this provider's placeholder. False is the "picked a provider, worker 403s"
// case, so the dashboard refuses the pick instead of letting it land broken.
func (s *Server) providerReachableFrom(p *store.PiProviderRec, slug string) (bool, string) {
	if p == nil {
		return false, "provider not found"
	}
	rec, found := s.secretForPiProvider(p.SecretID)
	if !found {
		return false, "this provider's secret is not on this gateway yet; save the provider again"
	}
	if !store.SecretVisibleToWorker(rec, slug) {
		return false, fmt.Sprintf(
			"the provider's secret is not granted to %s; grant it in Access → Secrets to make this override usable",
			slug)
	}
	return true, ""
}

func (s *Server) handleGetModelOverrides(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.modelOverrideListView())
}

func (s *Server) modelOverrideListView() modelOverrideListView {
	view := modelOverrideListView{Overrides: []modelOverrideView{}, Providers: []modelOverrideProviderView{}}
	if p := s.Store.PiProvider(); p != nil {
		view.Account = modelOverrideAccountView{
			ProviderID:   p.ID,
			ProviderName: piProviderLabel(p),
			Model:        effectivePiModel(p),
		}
	}
	for _, p := range s.Store.PiProviderStack() {
		rec := p
		view.Providers = append(view.Providers, modelOverrideProviderView{
			ID:           rec.ID,
			Name:         piProviderLabel(&rec),
			DefaultModel: effectivePiModel(&rec),
			Workers:      s.providerWorkers(&rec),
		})
	}
	for _, rec := range s.Store.ModelOverrides() {
		row := modelOverrideView{Slug: rec.Slug, ProviderID: rec.ProviderID, Source: "override"}
		if w, ok := s.Store.WorkerBySlug(rec.Slug); ok {
			row.Worker = w.Name
		}
		if ctx, ok := s.Store.WorkerCtxBySlug(rec.Slug); ok {
			row.LiveModel = ctx.Model
		}
		if !rec.UpdatedAt.IsZero() {
			row.UpdatedAt = rec.UpdatedAt.UTC().Format(time.RFC3339)
		}
		if p := s.Store.PiProviderByID(rec.ProviderID); p != nil {
			row.ProviderName = piProviderLabel(p)
			row.Model = effectivePiModel(p)
			if ok, why := s.providerReachableFrom(p, rec.Slug); !ok {
				row.Warning = why
			}
		} else {
			resolved := s.resolveWorkerModel(rec.Slug)
			row.Source = resolved.Source
			row.Warning = resolved.Warning
			if row.Warning == "" {
				row.Warning = "the pinned provider no longer exists; this worker uses the account default"
			}
		}
		view.Overrides = append(view.Overrides, row)
	}
	return view
}

// handlePutModelOverride pins one worker to one provider. Adding an override
// for a worker that already has one replaces it, so the list can never express
// "worker X on A and on B".
func (s *Server) handlePutModelOverride(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug       string `json:"slug"`
		ProviderID string `json:"providerId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	slug := strings.TrimSpace(in.Slug)
	if !workerNameRe.MatchString(slug) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	if _, ok := s.Store.WorkerBySlug(slug); !ok {
		writeJSON(w, 404, map[string]string{"error": "worker not found"})
		return
	}
	id := piprofile.CanonicalProviderID(in.ProviderID)
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "provider required"})
		return
	}
	// A legacy write (or an older coordinator) may still say "routstr"; the
	// stack stores it under the canonical id, so a lookup by raw id misses.
	p := s.Store.PiProviderByID(id)
	if p == nil {
		writeJSON(w, 400, map[string]string{"error": "provider " + id + " is not in the provider stack"})
		return
	}
	if ok, why := s.providerReachableFrom(p, slug); !ok {
		writeJSON(w, 400, map[string]string{"error": why})
		return
	}
	if _, err := s.Store.SetModelOverride(slug, id); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	view := s.modelOverrideListView()
	for _, row := range view.Overrides {
		if row.Slug == slug {
			writeJSON(w, 200, row)
			return
		}
	}
	writeJSON(w, 200, modelOverrideView{Slug: slug, ProviderID: id})
}

// handleDeleteModelOverride returns a worker to the account default.
func (s *Server) handleDeleteModelOverride(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if !workerNameRe.MatchString(slug) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	if err := s.Store.DeleteModelOverride(slug); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "slug": slug})
}

// handleWorkerGetModel answers the worker's own question: which provider should
// this session start on? Same identity rule as every other worker route — a
// worker may only ask about itself.
func (s *Server) handleWorkerGetModel(w http.ResponseWriter, r *http.Request) {
	worker := strings.TrimSpace(r.URL.Query().Get("worker"))
	if !workerNameRe.MatchString(worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	if !s.workerIdentityOK(w, r, worker) {
		return
	}
	slug := slugFromWorker(worker)
	if reg, ok := s.Store.WorkerByName(worker); ok && reg.Slug != "" {
		slug = reg.Slug
	}
	out := s.resolveWorkerModel(slug)
	out.Worker = worker
	out.Slug = slug
	writeJSON(w, 200, out)
}
