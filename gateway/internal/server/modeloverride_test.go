package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

// overrideTestServer is a gateway with two providers in the stack and one
// worker, so precedence (override → account) and scope rejection are testable
// without any network.
func overrideTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PISO_PI_PROFILE", filepath.Join(dir, "pi-profile"))
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"),
		filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	// "aiml" is granted to every worker (empty Workers). "scoped" is granted
	// only to "other", which is the case that must be refused up front.
	if err := st.AddSecret(store.SecretRec{
		ID: "sec_aiml", Name: "AIML", EnvKey: "PISO_AIML",
		Placeholder: "piso_aimlapi", Value: "sk-test-FAKE",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{
		ID: "sec_scoped", Name: "Scoped", EnvKey: "PISO_SCOPED",
		Placeholder: "piso_scoped", Value: "sk-test-FAKE",
		Workers: []string{"other"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPiProvider(store.PiProviderRec{
		ID: "piso-aiml", Name: "AIML", SecretID: "sec_aiml",
		BaseURL: "https://aiml.example.test/v1", DefaultModel: "model-a",
		Models: []string{"model-a", "model-b"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPiProviderActive(store.PiProviderRec{
		ID: "piso-scoped", Name: "Scoped", SecretID: "sec_scoped",
		BaseURL: "https://scoped.example.test/v1", DefaultModel: "model-s",
		Models: []string{"model-s"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertWorker(store.WorkerRec{Name: "piso-worker-demo", Slug: "demo", IPs: []string{"192.168.107.50"}}); err != nil {
		t.Fatal(err)
	}
	return &Server{Store: st, DataDir: dir}
}

func putOverride(t *testing.T, s *Server, slug, provider string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"slug":"` + slug + `","providerId":"` + provider + `"}`
	rec := httptest.NewRecorder()
	s.handlePutModelOverride(rec, httptest.NewRequest(http.MethodPut, "/api/v1/model-overrides", strings.NewReader(body)))
	return rec
}

func TestModelOverridePrecedence(t *testing.T) {
	s := overrideTestServer(t)

	// No override: the worker follows the account default.
	account := s.resolveWorkerModel("demo")
	if account.Source != "account" || account.Provider != "piso-scoped" || account.Model != "model-s" {
		t.Fatalf("account default: %+v", account)
	}

	// The provider granted to every worker is accepted.
	rec := putOverride(t, s, "demo", "piso-aiml")
	if rec.Code != 200 {
		t.Fatalf("put: status %d body %s", rec.Code, rec.Body.String())
	}
	got := s.resolveWorkerModel("demo")
	if got.Source != "override" || got.Provider != "piso-aiml" || got.Model != "model-a" {
		t.Fatalf("override: %+v", got)
	}

	// The account default moving underneath does not move the overridden
	// worker; it keeps its pin.
	if err := s.Store.ActivatePiProvider("piso-aiml"); err != nil {
		t.Fatal(err)
	}
	if got := s.resolveWorkerModel("demo"); got.Source != "override" || got.Provider != "piso-aiml" {
		t.Fatalf("after account change: %+v", got)
	}

	// Removing the override returns the worker to the (now changed) default.
	del := httptest.NewRecorder()
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/model-overrides/demo", nil)
	delReq.SetPathValue("slug", "demo")
	s.handleDeleteModelOverride(del, delReq)
	if del.Code != 200 {
		t.Fatalf("delete: status %d body %s", del.Code, del.Body.String())
	}
	if got := s.resolveWorkerModel("demo"); got.Source != "account" || got.Provider != "piso-aiml" {
		t.Fatalf("after delete: %+v", got)
	}
}

// A provider whose secret is not granted to the worker would authenticate
// nothing: the override is refused with the fix named, not stored broken.
func TestModelOverrideRejectsUnscopedProvider(t *testing.T) {
	s := overrideTestServer(t)
	rec := putOverride(t, s, "demo", "piso-scoped")
	if rec.Code != 400 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Access → Secrets") {
		t.Fatalf("error should name the fix: %s", rec.Body.String())
	}
	if rows := s.Store.ModelOverrides(); len(rows) != 0 {
		t.Fatalf("rejected override was stored: %+v", rows)
	}
}

func TestModelOverrideValidation(t *testing.T) {
	s := overrideTestServer(t)
	for _, tc := range []struct {
		name string
		slug string
		prov string
		code int
	}{
		{name: "unknown worker", slug: "nope", prov: "piso-aiml", code: 404},
		{name: "unknown provider", slug: "demo", prov: "piso-missing", code: 400},
		{name: "blank provider", slug: "demo", prov: "", code: 400},
		{name: "blank worker", slug: "", prov: "piso-aiml", code: 400},
	} {
		if rec := putOverride(t, s, tc.slug, tc.prov); rec.Code != tc.code {
			t.Fatalf("%s: status %d body %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if rows := s.Store.ModelOverrides(); len(rows) != 0 {
		t.Fatalf("invalid input was stored: %+v", rows)
	}
}

// A legacy id still resolves through the canonical namespace, matching what
// the extension does when it registers providers.
func TestModelOverrideCanonicalizesLegacyProvider(t *testing.T) {
	s := overrideTestServer(t)
	if err := s.Store.UpsertPiProvider(store.PiProviderRec{
		ID: "piso", Name: "Piso", SecretID: "sec_aiml",
		BaseURL: "https://piso.example.test/v1", DefaultModel: "model-p",
	}); err != nil {
		t.Fatal(err)
	}
	rec := putOverride(t, s, "demo", "routstr")
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if got := s.Store.ModelOverrideProviderID("demo"); got != "piso" {
		t.Fatalf("stored id: %q", got)
	}
}

// A provider deleted from the stack leaves the row visible (operator intent is
// preserved) but resolves to the account default instead of breaking.
func TestModelOverrideDanglingProviderFallsBack(t *testing.T) {
	s := overrideTestServer(t)
	if rec := putOverride(t, s, "demo", "piso-aiml"); rec.Code != 200 {
		t.Fatalf("put: status %d body %s", rec.Code, rec.Body.String())
	}
	if err := s.Store.DeletePiProvider("piso-aiml"); err != nil {
		t.Fatal(err)
	}
	got := s.resolveWorkerModel("demo")
	if got.Source != "account" || got.Provider != "piso-scoped" {
		t.Fatalf("fallback: %+v", got)
	}
	if got.Warning == "" {
		t.Fatal("dangling override should warn")
	}
	rec := httptest.NewRecorder()
	s.handleGetModelOverrides(rec, httptest.NewRequest(http.MethodGet, "/api/v1/model-overrides", nil))
	if !strings.Contains(rec.Body.String(), `"slug":"demo"`) {
		t.Fatalf("row should survive a deleted provider: %s", rec.Body.String())
	}
}

func TestWorkerModelEndpoint(t *testing.T) {
	s := overrideTestServer(t)
	if rec := putOverride(t, s, "demo", "piso-aiml"); rec.Code != 200 {
		t.Fatalf("put: status %d body %s", rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	s.handleWorkerGetModel(rec, httptest.NewRequest(http.MethodGet, "/api/v1/worker/model?worker=piso-worker-demo", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var out workerModelView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Provider != "piso-aiml" || out.Model != "model-a" || out.Source != "override" || out.Slug != "demo" {
		t.Fatalf("worker view: %+v", out)
	}
	// The response is the worker-facing shape: no host paths, no secrets.
	if strings.Contains(rec.Body.String(), "sk-test") || strings.Contains(rec.Body.String(), s.DataDir) {
		t.Fatalf("leaked: %s", rec.Body.String())
	}
}

// A remote provider with no default and no approved models has nothing to pin
// to: every generation through it answers 403 model-pin-unresolved, so the
// override is refused up front with the fix named.
func TestModelOverrideRejectsProviderWithNoModels(t *testing.T) {
	s := overrideTestServer(t)
	if err := s.Store.UpsertPiProvider(store.PiProviderRec{
		ID: "piso-empty", Name: "Empty", SecretID: "sec_aiml",
		BaseURL: "https://empty.example.test/v1",
	}); err != nil {
		t.Fatal(err)
	}
	rec := putOverride(t, s, "demo", "piso-empty")
	if rec.Code != 400 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "default model") {
		t.Fatalf("error should name the fix: %s", rec.Body.String())
	}
	if rows := s.Store.ModelOverrides(); len(rows) != 0 {
		t.Fatalf("rejected override was stored: %+v", rows)
	}

	// Approving one model is the documented fix, and the retry then succeeds.
	if err := s.Store.UpsertPiProvider(store.PiProviderRec{
		ID: "piso-empty", Name: "Empty", SecretID: "sec_aiml",
		BaseURL: "https://empty.example.test/v1", ApprovedModels: []string{"model-e"},
	}); err != nil {
		t.Fatal(err)
	}
	if rec := putOverride(t, s, "demo", "piso-empty"); rec.Code != 200 {
		t.Fatalf("after approving a model: status %d body %s", rec.Code, rec.Body.String())
	}
}

// A local (Tinfoil) provider is exempt: the enclave proxy inside the worker
// serves it, so the gateway never judges the model and the worker still starts
// on whatever the profile offers.
func TestModelOverrideAllowsLocalProviderWithoutModels(t *testing.T) {
	s := overrideTestServer(t)
	if err := s.Store.UpsertPiProvider(store.PiProviderRec{
		ID: "piso-tinfoil", Name: "Tinfoil", SecretID: "sec_aiml",
		BaseURL: tinfoilProxyBaseURL, Kind: piProviderKindTinfoil,
	}); err != nil {
		t.Fatal(err)
	}
	if rec := putOverride(t, s, "demo", "piso-tinfoil"); rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if got := s.Store.ModelOverrideProviderID("demo"); got != "piso-tinfoil" {
		t.Fatalf("stored id: %q", got)
	}
}

// The override list is what the dashboard renders the add/edit dialog from, so
// it has to carry enough to recognise a local provider and to disable a pick
// that the save path would refuse.
func TestModelOverrideListDescribesProviders(t *testing.T) {
	s := overrideTestServer(t)
	if err := s.Store.UpsertPiProvider(store.PiProviderRec{
		ID: "piso-tinfoil", Name: "Tinfoil", SecretID: "sec_aiml",
		BaseURL: tinfoilProxyBaseURL, Kind: piProviderKindTinfoil,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpsertPiProvider(store.PiProviderRec{
		ID: "piso-empty", Name: "Empty", SecretID: "sec_aiml",
		BaseURL: "https://empty.example.test/v1",
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleGetModelOverrides(rec, httptest.NewRequest(http.MethodGet, "/api/v1/model-overrides", nil))
	var out modelOverrideListView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]modelOverrideProviderView{}
	for _, p := range out.Providers {
		byID[p.ID] = p
	}
	if p := byID["piso-tinfoil"]; p.Kind != piProviderKindTinfoil || !p.Pinnable {
		t.Fatalf("local provider: %+v", p)
	}
	if p := byID["piso-empty"]; p.Pinnable {
		t.Fatalf("provider with no models must report not pinnable: %+v", p)
	}
	// The remote provider the tests pin to is usable, and says so.
	if p := byID["piso-aiml"]; !p.Pinnable || p.BaseURL != "https://aiml.example.test/v1" {
		t.Fatalf("aiml provider: %+v", p)
	}
	if got := byID["piso-aiml"].AllowedModels; len(got) != 1 || got[0] != "model-a" {
		t.Fatalf("allowed models: %+v", got)
	}
}

// A pin saved against a usable provider can outlive that provider's models: the
// default is cleared, the approved list emptied. Drift is reported, never
// repaired, so the row stays and names the 403 it now produces instead of
// disappearing or reading as healthy.
func TestModelOverrideListWarnsWhenProviderLostItsModels(t *testing.T) {
	s := overrideTestServer(t)
	if rec := putOverride(t, s, "demo", "piso-aiml"); rec.Code != 200 {
		t.Fatalf("pin: status %d body %s", rec.Code, rec.Body.String())
	}
	if err := s.Store.UpsertPiProvider(store.PiProviderRec{
		ID: "piso-aiml", Name: "AIML", SecretID: "sec_aiml",
		BaseURL: "https://aiml.example.test/v1",
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleGetModelOverrides(rec, httptest.NewRequest(http.MethodGet, "/api/v1/model-overrides", nil))
	var out modelOverrideListView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Overrides) != 1 {
		t.Fatalf("overrides: %+v", out.Overrides)
	}
	row := out.Overrides[0]
	if row.Slug != "demo" || row.ProviderID != "piso-aiml" {
		t.Fatalf("row kept: %+v", row)
	}
	if !strings.Contains(row.Warning, "no usable model now") {
		t.Fatalf("warning: %q", row.Warning)
	}
}
