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
