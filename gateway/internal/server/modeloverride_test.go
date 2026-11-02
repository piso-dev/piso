package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
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

// /model may only offer what the pin will accept. The pin is a per-worker
// boundary: modelPin compares the request host against the one provider the
// worker is pinned to, so a model approved on a *different* remote provider is
// still refused as model-provider-mismatch. Publishing that provider's allowlist
// would move a dashboard policy into a mid-session 403, which is the bug this
// test exists for.
//
// The two shapes have to stay distinguishable over the wire: a provider this
// worker cannot use is present and empty, while a provider the gateway never
// answered for is absent.
func TestWorkerModelPublishesOnlyUsableProviders(t *testing.T) {
	s := overrideTestServer(t)
	for _, rec := range []store.PiProviderRec{
		{
			ID: "piso-empty", Name: "Empty", SecretID: "sec_aiml",
			BaseURL: "https://empty.example.test/v1",
		},
		{
			// Has its own default model, so the tab genuinely allows one id —
			// and this worker still cannot run it, because it is not pinned
			// here. Offering it was the reported 403.
			ID: "piso-approved", Name: "Approved", SecretID: "sec_aiml",
			BaseURL:      "https://approved.example.test/v1",
			DefaultModel: "model-x", ApprovedModels: []string{"model-y"},
		},
	} {
		if err := s.Store.UpsertPiProvider(rec); err != nil {
			t.Fatal(err)
		}
	}
	view := func() workerModelView {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handleWorkerGetModel(rec, httptest.NewRequest(http.MethodGet, "/api/v1/worker/model?worker=piso-worker-demo", nil))
		if rec.Code != 200 {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		var out workerModelView
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// The account default is piso-scoped, so that is where this worker's
	// requests go and the only remote catalog it may see.
	out := view()
	if out.Provider != "piso-scoped" {
		t.Fatalf("account default should pin the worker: %+v", out)
	}
	if got := out.Providers["piso-scoped"]; len(got) != 1 || got[0] != "model-s" {
		t.Fatalf("pinned provider must keep its allowlist: %#v", got)
	}
	for _, id := range []string{"piso-aiml", "piso-approved", "piso-empty"} {
		got, present := out.Providers[id]
		if !present || got == nil || len(got) != 0 {
			t.Fatalf("%s is configured but unpinned, so it must be present and empty: %#v (present %v)", id, got, present)
		}
	}
	// The map and the older single-provider field must never disagree.
	if !slices.Equal(out.Providers[out.Provider], out.AllowedModels) {
		t.Fatalf("allowedModels %#v disagrees with providers[%s] %#v", out.AllowedModels, out.Provider, out.Providers[out.Provider])
	}
	// A provider outside the stack is not answered for at all.
	if _, present := out.Providers["piso-not-in-the-stack"]; present {
		t.Fatal("a provider outside the stack must not be answered for")
	}

	// Pinning this worker elsewhere moves the usable catalog with it, in both
	// fields and in one answer.
	if rec := putOverride(t, s, "demo", "piso-approved"); rec.Code != 200 {
		t.Fatalf("pin: status %d body %s", rec.Code, rec.Body.String())
	}
	out = view()
	if out.Provider != "piso-approved" {
		t.Fatalf("override should win: %+v", out)
	}
	if got := out.Providers["piso-approved"]; len(got) != 2 || got[0] != "model-y" || got[1] != "model-x" {
		t.Fatalf("newly pinned provider: %#v", got)
	}
	if got := out.Providers["piso-scoped"]; len(got) != 0 {
		t.Fatalf("the previously pinned provider must go empty: %#v", got)
	}

	// The worker only sees JSON, so an empty answer has to survive as [] rather
	// than null or an omitted key.
	var raw map[string]any
	rec := httptest.NewRecorder()
	s.handleWorkerGetModel(rec, httptest.NewRequest(http.MethodGet, "/api/v1/worker/model?worker=piso-worker-demo", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	providers, ok := raw["providers"].(map[string]any)
	if !ok {
		t.Fatalf("providers missing from the wire: %s", rec.Body.String())
	}
	if list, ok := providers["piso-empty"].([]any); !ok || len(list) != 0 {
		t.Fatalf("empty allowlist must marshal as []: %#v", providers["piso-empty"])
	}
}

// A local provider's requests never reach the proxy — its base URL is loopback,
// which NO_PROXY keeps off it — so modelPin cannot judge it. The worker
// extension is therefore the only place the pin can be enforced for one, and
// the answer it reads is this one. An unpinned local provider is denied exactly
// like a remote one: leaving it in the menu let a worker the dashboard said was
// running on another provider generate through Tinfoil.
func TestWorkerModelDeniesLocalProvidersWhenUnpinned(t *testing.T) {
	s := overrideTestServer(t)
	if err := s.Store.UpsertPiProvider(store.PiProviderRec{
		ID: "piso-tinfoil", Name: "Tinfoil", SecretID: "sec_aiml",
		BaseURL: tinfoilProxyBaseURL, Kind: piProviderKindTinfoil,
		DefaultModel: "llama-local",
	}); err != nil {
		t.Fatal(err)
	}
	view := func() workerModelView {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handleWorkerGetModel(rec, httptest.NewRequest(http.MethodGet, "/api/v1/worker/model?worker=piso-worker-demo", nil))
		if rec.Code != 200 {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		var out workerModelView
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := view()
	if out.Provider == "piso-tinfoil" {
		t.Fatalf("the account default is remote, not tinfoil: %+v", out)
	}
	if got, present := out.Providers["piso-tinfoil"]; !present || len(got) != 0 {
		t.Fatalf("an unpinned local provider must be a known denial: %#v (present %v)", got, present)
	}
	if policy := out.ModelPolicies["piso-tinfoil"]; policy.Mode != "unavailable" || len(policy.Models) != 0 {
		t.Fatalf("an unpinned local provider must be unavailable: %+v", policy)
	}
	// Pinning it is what makes it usable again, in the same answer.
	if rec := putOverride(t, s, "demo", "piso-tinfoil"); rec.Code != 200 {
		t.Fatalf("pin: status %d body %s", rec.Code, rec.Body.String())
	}
	out = view()
	if out.Provider != "piso-tinfoil" || out.Model != "llama-local" {
		t.Fatalf("a pinned local provider keeps its default: %+v", out)
	}
	if got := out.Providers["piso-tinfoil"]; len(got) != 1 || got[0] != "llama-local" {
		t.Fatalf("a pinned local provider keeps its allowlist: %#v", got)
	}
	if policy := out.ModelPolicies["piso-tinfoil"]; policy.Mode != "restricted" || policy.Discovery != "worker" {
		t.Fatalf("a pinned local provider is restricted, not denied: %+v", policy)
	}
	// The remote provider it replaced is now the denial, whichever kind it is.
	if got, present := out.Providers["piso-scoped"]; !present || len(got) != 0 {
		t.Fatalf("the previous pin must go empty: %#v (present %v)", got, present)
	}
}

// With no providers tab there is nothing to mirror, so the field is omitted and
// a worker keeps the behaviour it had before the field existed.
func TestWorkerModelOmitsProvidersWithoutAStack(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"),
		filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, DataDir: dir}
	rec := httptest.NewRecorder()
	s.handleWorkerGetModel(rec, httptest.NewRequest(http.MethodGet, "/api/v1/worker/model?worker=piso-worker-demo", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"providers"`) {
		t.Fatalf("an empty stack must omit providers: %s", rec.Body.String())
	}
}
