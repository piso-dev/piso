package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

func putOverrideModel(t *testing.T, s *Server, slug, provider, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"slug":"` + slug + `","providerId":"` + provider + `","model":"` + model + `"}`
	rec := httptest.NewRecorder()
	s.handlePutModelOverride(rec, httptest.NewRequest(http.MethodPut, "/api/v1/model-overrides", strings.NewReader(body)))
	return rec
}

func TestModelOverridePinsModelWithinProvider(t *testing.T) {
	s := overrideTestServer(t)
	// model-b is in the catalog but not approved: the pin is refused up front
	// rather than stored as a 403 waiting to happen.
	rec := putOverrideModel(t, s, "demo", "piso-aiml", "model-b")
	if rec.Code != 400 {
		t.Fatalf("unapproved pin: status %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model-a") {
		t.Fatalf("refusal should name the allowed models: %s", rec.Body.String())
	}

	// Approving it makes the same pin acceptable.
	p := s.Store.PiProviderByID("piso-aiml")
	p.ApprovedModels = []string{"model-a", "model-b"}
	if err := s.Store.UpsertPiProvider(*p); err != nil {
		t.Fatal(err)
	}
	if rec := putOverrideModel(t, s, "demo", "piso-aiml", "model-b"); rec.Code != 200 {
		t.Fatalf("approved pin: status %d body %s", rec.Code, rec.Body.String())
	}
	if got := s.resolveWorkerModel("demo"); got.Model != "model-b" || got.Source != "override" {
		t.Fatalf("resolved: %+v", got)
	}

	// An empty model means "the provider's default", so the row survives a
	// later change of default instead of freezing a copy of it.
	if rec := putOverrideModel(t, s, "demo", "piso-aiml", ""); rec.Code != 200 {
		t.Fatalf("blank model: status %d body %s", rec.Code, rec.Body.String())
	}
	if got := s.resolveWorkerModel("demo"); got.Model != "model-a" {
		t.Fatalf("blank model should follow the provider default: %+v", got)
	}
}

// A drifted pin falls back to the provider's default and says so: a worker that
// refuses to start is worse than one running a documented substitute the
// dashboard is already flagging.
func TestModelOverridePinDriftFallsBack(t *testing.T) {
	s := overrideTestServer(t)
	p := s.Store.PiProviderByID("piso-aiml")
	p.ApprovedModels = []string{"model-a", "model-b"}
	if err := s.Store.UpsertPiProvider(*p); err != nil {
		t.Fatal(err)
	}
	if rec := putOverrideModel(t, s, "demo", "piso-aiml", "model-b"); rec.Code != 200 {
		t.Fatalf("pin: status %d body %s", rec.Code, rec.Body.String())
	}

	// No catalog checked yet: not knowing is not a reason to substitute.
	if got := s.resolveWorkerModel("demo"); got.Model != "model-b" {
		t.Fatalf("unchecked catalog dropped the pin: %+v", got)
	}

	// The provider drops model-b from its catalog.
	if err := s.Store.SetPiProviderCatalog("piso-aiml", []string{"model-a"}, ""); err != nil {
		t.Fatal(err)
	}
	got := s.resolveWorkerModel("demo")
	if got.Model != "model-a" {
		t.Fatalf("drifted pin did not fall back: %+v", got)
	}
	if !strings.Contains(got.Warning, "model-b") {
		t.Fatalf("fallback should name the pinned model: %q", got.Warning)
	}

	// A failed catalog check is not drift: the pin survives an outage.
	if err := s.Store.SetPiProviderCatalog("piso-aiml", nil, "provider model catalog is unavailable"); err != nil {
		t.Fatal(err)
	}
	if got := s.resolveWorkerModel("demo"); got.Model != "model-b" {
		t.Fatalf("failed check dropped the pin: %+v", got)
	}
}

func TestModelOverrideViewReportsDrift(t *testing.T) {
	s := overrideTestServer(t)
	p := s.Store.PiProviderByID("piso-aiml")
	p.ApprovedModels = []string{"model-a", "model-b"}
	if err := s.Store.UpsertPiProvider(*p); err != nil {
		t.Fatal(err)
	}
	if rec := putOverrideModel(t, s, "demo", "piso-aiml", "model-b"); rec.Code != 200 {
		t.Fatalf("pin: status %d body %s", rec.Code, rec.Body.String())
	}
	if err := s.Store.SetPiProviderCatalog("piso-aiml", []string{"model-a"}, ""); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.handleGetModelOverrides(rec, httptest.NewRequest(http.MethodGet, "/api/v1/model-overrides", nil))
	var view modelOverrideListView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Overrides) != 1 {
		t.Fatalf("overrides: %+v", view.Overrides)
	}
	row := view.Overrides[0]
	if !row.Drifted || row.PinnedModel != "model-b" || row.Model != "model-a" {
		t.Fatalf("row: %+v", row)
	}
	if row.Warning == "" {
		t.Fatal("a drifted row must explain the substitution")
	}
	// The dropdown is built from the approved set, not the whole catalog.
	for _, provider := range view.Providers {
		if provider.ID != "piso-aiml" {
			continue
		}
		if strings.Join(provider.AllowedModels, ",") != "model-a,model-b" {
			t.Fatalf("allowed models: %v", provider.AllowedModels)
		}
	}
}

// The dashboard needs the verdicts, not just the catalog: computing them here
// keeps one definition of drift for the card, the row, and the worker.
func TestPiProviderViewCarriesDrift(t *testing.T) {
	s := overrideTestServer(t)
	p := s.Store.PiProviderByID("piso-aiml")
	p.ApprovedModels = []string{"model-a", "model-b", "model-gone"}
	if err := s.Store.UpsertPiProvider(*p); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetPiProviderCatalog("piso-aiml", []string{"model-a", "model-b"}, ""); err != nil {
		t.Fatal(err)
	}

	view := s.providerViewFor(s.Store.PiProviderByID("piso-aiml"), profileFileState{})
	if view.Drift == nil {
		t.Fatal("no drift report")
	}
	if view.Drift.Missing != 1 || view.Drift.Total != 3 {
		t.Fatalf("counts: %+v", view.Drift)
	}
	var gone store.ModelDrift
	for _, item := range view.Drift.Items {
		if item.Model == "model-gone" {
			gone = item
		}
	}
	if gone.State != store.ModelStateMissing {
		t.Fatalf("model-gone: %+v", gone)
	}
	// A verdict we could make does not need an excuse.
	if view.DriftUnknown != "" {
		t.Fatalf("driftUnknown: %q", view.DriftUnknown)
	}

	// A local provider can never be checked by the gateway, and must not be
	// reported as drifted for it.
	local := s.Store.PiProviderByID("piso-aiml")
	local.Kind = piProviderKindTinfoil
	local.BaseURL = tinfoilProxyBaseURL
	view = s.providerViewFor(local, profileFileState{})
	if view.DriftUnknown == "" {
		t.Fatal("a local provider should say why it cannot be checked")
	}
	for _, item := range view.Drift.Items {
		if item.State != store.ModelStateUnknown {
			t.Fatalf("local provider reported %q for %s", item.State, item.Model)
		}
	}
}
