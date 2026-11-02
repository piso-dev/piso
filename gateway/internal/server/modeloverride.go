package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
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
	// PinnedModel is the model the row asked for. It differs from Model when
	// the pin was dropped — see Drifted.
	PinnedModel string `json:"pinnedModel,omitempty"`
	// LiveModel is what the worker last reported running (advisory; it is a
	// different thing from the configured selection).
	LiveModel string `json:"liveModel,omitempty"`
	Source    string `json:"source"`
	// Drifted says the pinned model is no longer in the provider's catalog and
	// the row fell back to the provider's default. Model then names the
	// substitute, so the row can show both without a second lookup.
	Drifted bool   `json:"drifted,omitempty"`
	Warning string `json:"warning,omitempty"`
	// DriftState is the pinned model's verdict: ok, missing, or unknown.
	DriftState string `json:"driftState,omitempty"`
	UpdatedAt  string `json:"updatedAt,omitempty"`
}

// modelOverrideProviderView feeds the provider dropdown. Workers is the
// provider secret's scope (empty = every worker), so the dashboard can say up
// front that a pick will not authenticate from a worker outside that list.
type modelOverrideProviderView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name,omitempty"`
	DefaultModel string   `json:"defaultModel,omitempty"`
	Workers      []string `json:"workers,omitempty"`
	// AllowedModels is the set a pin may name, so the dashboard's model
	// dropdown offers exactly what the gateway will accept instead of the
	// provider's whole catalog.
	AllowedModels []string `json:"allowedModels,omitempty"`
	Models        []string `json:"models,omitempty"`
	// Kind and BaseURL let the override form recognise a local (Tinfoil)
	// provider without loading the whole provider list: its model field is
	// optional because the enclave proxy inside the worker picks the model and
	// the gateway's pin never sees the request. BaseURL is carried because a
	// provider saved before kinds existed is local by URL alone.
	Kind    string `json:"kind,omitempty"`
	BaseURL string `json:"baseUrl,omitempty"`
	// Pinnable is false when the provider has nothing to pin to: no default, no
	// approved model, no catalog. A pin there would 403 every generation, so
	// the save path refuses it and the dashboard greys the option out.
	Pinnable bool `json:"pinnable"`
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
	// AllowedModels is the closed set this worker's requests may name. The
	// worker filters its /model picker to it: models.json lists a provider's
	// whole catalog, and offering ids the gateway will refuse turns a policy
	// into a 403 in the middle of a session. Omitted when empty so an older
	// worker keeps its current behaviour.
	AllowedModels []string `json:"allowedModels,omitempty"`
	// Providers carries the same answer for every provider in the stack, keyed
	// by provider id, because /model draws its list from the Model providers
	// tab rather than from one pinned provider. A present but empty list is the
	// tab saying "configured with nothing usable"; an absent id means this
	// gateway did not answer for it, and the worker then keeps the catalog it
	// always had. Omitting the field entirely is how an older gateway stays
	// readable, so it is omitted only when the stack is empty.
	Providers     map[string][]string          `json:"providers,omitempty"`
	ModelPolicies map[string]workerModelPolicy `json:"modelPolicies,omitempty"`
}

// usableAllowlists is what this worker may actually run, per provider.
//
// Not every provider in the tab is one of them. The pin is a per-worker
// boundary, not a per-provider preference: modelPin compares the request host
// against the *one* provider the worker is pinned to and refuses every other
// host as model-provider-mismatch, because substitution rules are per-host and a
// request elsewhere would not be authenticated the way the pin assumes. So a
// model approved on a second remote provider is a model this worker cannot run,
// and offering it in /model only moves the dashboard's policy to a 403 in the
// middle of a session.
//
// A local (Tinfoil) provider is the exception and not as a kindness: its base URL
// is loopback, which NO_PROXY keeps off the proxy, so modelPin never sees its
// requests and the enclave proxy inside the worker picks the model. Its approved
// list is the only thing standing between the worker and a model, so it stays.
//
// Empty means a known denial. Local providers without explicit approvals are
// omitted: their catalog is worker-discovered, not a gateway-visible empty set.
func (s *Server) usableAllowlists(pinned string) map[string][]string {
	stack := s.Store.PiProviderStack()
	if len(stack) == 0 {
		return nil
	}
	out := make(map[string][]string, len(stack))
	for i := range stack {
		p := &stack[i]
		if p.ID == "" {
			continue
		}
		if p.ID != pinned && !piProviderIsLocal(p) {
			out[p.ID] = []string{}
			continue
		}
		if piProviderIsLocal(p) {
			ids := localApprovedModels(p)
			if len(ids) == 0 {
				continue
			} // discovery, not a denial
			out[p.ID] = ids
		} else {
			out[p.ID] = p.AllowedModelIDs()
		}
	}
	return out
}

// resolveWorkerModel applies the precedence in one place: a worker override
// wins over the account default, and an override pointing at a provider that no
// longer exists falls back to the account default with a warning rather than
// leaving the worker with nothing.
//
// Within a pinned provider, an override's model wins over that provider's
// default, and a pinned model the catalog no longer lists falls back to the
// provider's default with a warning. The fallback is deliberate: a drifted pin
// is a config bug, and a worker that refuses to start is a worse outcome than
// one that runs a documented substitute the dashboard is already flagging.
func (s *Server) resolveWorkerModel(slug string) workerModelView {
	slug = strings.TrimSpace(slug)
	out := workerModelView{Slug: slug, Source: "none"}
	rec, hasOverride := s.Store.ModelOverrideBySlug(slug)
	if p := s.Store.PiProviderByID(rec.ProviderID); hasOverride && p != nil {
		out.Provider = p.ID
		out.Model = effectivePiModel(p)
		out.Source = "override"
		out.AllowedModels = p.AllowedModelIDs()
		if piProviderIsLocal(p) {
			out.AllowedModels = localApprovedModels(p)
		}
		if pinned := strings.TrimSpace(rec.Model); pinned != "" {
			switch s.pinnedModelDrift(p, pinned) {
			case "":
				out.Model = pinned
			case "model-not-approved":
				out.Warning = fmt.Sprintf(
					"the pinned model %s is not an approved model for %s; this worker uses %s instead",
					pinned, piProviderLabel(p), out.Model)
			default:
				out.Warning = fmt.Sprintf(
					"the pinned model %s is no longer offered by %s; this worker uses %s instead",
					pinned, piProviderLabel(p), out.Model)
			}
		}
		out.Providers = s.usableAllowlists(out.Provider)
		out.ModelPolicies = s.workerModelPolicies(out.Provider, slug)
		return out
	}
	if hasOverride {
		out.Warning = "the pinned provider no longer exists; this worker uses the account default"
	}
	if p := s.Store.PiProvider(); p != nil {
		out.Provider = p.ID
		out.Model = effectivePiModel(p)
		out.Source = "account"
		out.AllowedModels = p.AllowedModelIDs()
		if piProviderIsLocal(p) {
			out.AllowedModels = localApprovedModels(p)
		}
	}
	out.Providers = s.usableAllowlists(out.Provider)
	out.ModelPolicies = s.workerModelPolicies(out.Provider, slug)
	return out
}

// pinnedModelDrift returns a non-empty reason when model cannot be used under p,
// or "" when it can. Two different failures, deliberately kept apart:
//
//   - "model-not-approved": the provider's own approved list excludes it, so
//     every request would be refused no matter what the catalog says. Only an
//     older row (or a hand-edited state file) can be in this state, since the
//     API validates the pin on save.
//   - "model-not-in-catalog": the list accepts it but the provider stopped
//     offering it.
//
// Neither is a reason to substitute when the catalog could not be checked: an
// unanswered question must not rewrite the config on a guess.
func (s *Server) pinnedModelDrift(p *store.PiProviderRec, model string) string {
	if p == nil || model == "" {
		return ""
	}
	if piProviderIsLocal(p) {
		ids := localApprovedModels(p)
		if len(ids) == 0 || slices.Contains(ids, model) {
			return ""
		}
		return "model-not-approved"
	}
	approved := slices.Contains(p.AllowedModelIDs(), model)
	report := store.ModelDriftFor(p, map[string]string{"pinned": model})
	if report.Error != "" || report.Suspect || report.CheckedAt == "" {
		// The catalog is silent, so only the approved list can rule the model
		// out — and if it does, the pin is simply wrong.
		if !approved {
			return "model-not-approved"
		}
		return ""
	}
	if approved && store.ModelDriftState(report, model).State != store.ModelStateMissing {
		return ""
	}
	if !approved {
		return "model-not-approved"
	}
	return "model-not-in-catalog"
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
			ID:            rec.ID,
			Name:          piProviderLabel(&rec),
			DefaultModel:  effectivePiModel(&rec),
			Workers:       s.providerWorkers(&rec),
			AllowedModels: rec.AllowedModelIDs(),
			Models:        s.localCatalogModels(&rec),
			Kind:          rec.Kind,
			BaseURL:       rec.BaseURL,
			Pinnable:      piProviderPinnable(&rec),
		})
	}
	for _, rec := range s.Store.ModelOverrides() {
		row := modelOverrideView{Slug: rec.Slug, ProviderID: rec.ProviderID, PinnedModel: rec.Model, Source: "override"}
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
			// The provider was pinnable when this row was saved and has lost its
			// models since (the default was cleared, the approved list emptied).
			// Keeping the row and saying so is the same choice made for a removed
			// provider: a report, never a repair — the fix is a provider edit, and
			// deleting the override would only hide the 403.
			if !piProviderPinnable(p) && row.Warning == "" {
				row.Warning = fmt.Sprintf(
					"%s has no usable model now, so every request through this override is refused; "+
						"set a default model or approve one for it", piProviderLabel(p))
			}
			// The resolved answer and the drift verdict come from the same
			// resolver the worker uses, so a row can never claim a model the
			// worker will not actually run.
			resolved := s.resolveWorkerModel(rec.Slug)
			row.Model = resolved.Model
			row.DriftState = store.ModelDriftState(store.ModelDriftFor(p, nil), rec.Model).State
			if rec.Model != "" && resolved.Model != rec.Model {
				row.Drifted = true
				if row.Warning == "" {
					row.Warning = resolved.Warning
				}
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
		Model      string `json:"model"`
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
	// A pin to a provider with nothing to pin to is an outage rather than a
	// preference: every generation answers 403 model-pin-unresolved and the
	// worker starts on no model at all. Unlike a drifting catalog this is
	// entirely knowable here, so it is refused with the fix named instead of
	// stored and left for the worker's next poll to discover.
	if !piProviderPinnable(p) {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf(
			"%s has no usable model yet, so every request through it would be refused; "+
				"set a default model or approve at least one model for it first", piProviderLabel(p))})
		return
	}
	// A pin outside the provider's approved set is refused rather than saved
	// with a warning: the worker would 403 the moment it switched, and unlike
	// the catalog this set is right here in the request. The extra model is a
	// dashboard edit away, which is the point of the split.
	model := strings.TrimSpace(in.Model)
	if model != "" {
		allowed := p.AllowedModelIDs()
		if piProviderIsLocal(p) {
			allowed = localApprovedModels(p)
		}
		if !(piProviderIsLocal(p) && len(allowed) == 0) && !slices.Contains(allowed, model) {
			writeJSON(w, 400, map[string]string{"error": fmt.Sprintf(
				"%s is not an approved model for %s; pick one of: %s",
				model, piProviderLabel(p), strings.Join(allowed, ", "))})
			return
		}
	}
	if _, err := s.Store.SetModelOverride(slug, id, model); err != nil {
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

// piProviderPinnable reports whether a worker may pin to this provider. A local
// provider always may: its traffic goes to the enclave proxy on the worker's own
// loopback, so the gateway never judges the model and an empty set costs
// nothing. A remote one needs at least one usable id, or the pin can only ever
// answer model-pin-unresolved.
func piProviderPinnable(p *store.PiProviderRec) bool {
	if p == nil {
		return false
	}
	if piProviderIsLocal(p) {
		return true
	}
	return len(p.AllowedModelIDs()) > 0
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
