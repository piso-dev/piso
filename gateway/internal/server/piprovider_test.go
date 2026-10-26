package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
)

func TestPutPiProviderWritesPlaceholderNotValue(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "pi-profile")
	t.Setenv("PISO_PI_PROFILE", profile)
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	const real = "sk-test-FAKE-not-written"
	if err := st.AddSecret(store.SecretRec{
		ID: "sec_local", Name: "AIML", Placeholder: "piso_aimlapi", Value: real,
		TechnocoreID: "11111111-1111-1111-1111-111111111111",
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, DataDir: dir}
	body := []byte(`{"secretId":"11111111-1111-1111-1111-111111111111","baseUrl":"https://api.example.test/v1"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/pi-provider", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handlePutPiProvider(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var view piProviderView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Applied || view.Placeholder != "piso_aimlapi" {
		t.Fatalf("view: %+v", view)
	}
	if strings.Contains(rec.Body.String(), real) {
		t.Fatalf("response leaked the secret: %s", rec.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(profile, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if strings.Contains(got, real) {
		t.Fatalf("models.json contains the real key: %s", got)
	}
	if !strings.Contains(got, `"apiKey": "piso_aimlapi"`) || !strings.Contains(got, `"models": []`) {
		t.Fatalf("profile: %s", got)
	}
	if st.PiProvider().SecretID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("stored pointer: %+v", st.PiProvider())
	}
}

// providerTestServer is a configured provider whose catalog is served by fake.
func providerTestServer(t *testing.T, fake *httptest.Server) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	profile := filepath.Join(dir, "pi-profile")
	t.Setenv("PISO_PI_PROFILE", profile)
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	const real = "sk-test-FAKE-not-written"
	if err := st.AddSecret(store.SecretRec{
		ID: "sec_local", Name: "AIML", Placeholder: "piso_aimlapi", Value: real,
		TechnocoreID: "11111111-1111-1111-1111-111111111111",
	}); err != nil {
		t.Fatal(err)
	}
	return &Server{Store: st, DataDir: dir, HTTP: fake.Client()}, profile
}

func TestPiProviderModelsListsIDsOnly(t *testing.T) {
	const real = "sk-test-FAKE-not-written"
	var gotAuth string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/models" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"model-a"},{"id":"model-b"},{"id":"model-a"},{"id":"  "},{"secret":"hidden"}]}`)
	}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	s.Store.SetPiProvider(&store.PiProviderRec{SecretID: "11111111-1111-1111-1111-111111111111", BaseURL: fake.URL + "/v1"})

	rec := httptest.NewRecorder()
	s.handleGetPiProviderModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pi-provider/models", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), real) || strings.Contains(rec.Body.String(), "hidden") {
		t.Fatalf("response leaked provider data: %s", rec.Body.String())
	}
	var catalog piProviderCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if strings.Join(catalog.Models, ",") != "model-a,model-b" {
		t.Fatalf("models: %v", catalog.Models)
	}
	if gotAuth != "Bearer "+real {
		t.Fatalf("auth header %q", gotAuth)
	}
}

func TestPutPiProviderDefaultModel(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"model-a"},{"id":"model-b"}]}`)
	}))
	defer fake.Close()
	s, profile := providerTestServer(t, fake)

	put := func(id, model string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"id":%q,"secretId":"11111111-1111-1111-1111-111111111111","baseUrl":%q,"defaultModel":%q}`, id, fake.URL+"/v1", model)
		rec := httptest.NewRecorder()
		s.handlePutPiProvider(rec, httptest.NewRequest(http.MethodPut, "/api/v1/pi-provider", strings.NewReader(body)))
		return rec
	}
	// A typo is saved, not rejected: the catalog is advisory and the save
	// path never depends on it.
	first := put("", "not-a-model")
	if first.Code != 200 {
		t.Fatalf("unknown model: status %d body %s", first.Code, first.Body.String())
	}
	var view piProviderView
	if err := json.Unmarshal(first.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ID == "" {
		t.Fatalf("missing generated id: %s", first.Body.String())
	}
	if rec := put(view.ID, "model-b"); rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(profile, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"defaultProvider": "`+view.ID+`"`) || !strings.Contains(string(raw), `"defaultModel": "model-b"`) {
		t.Fatalf("settings: %s", raw)
	}
	// Other settings survive, and clearing the default removes both keys.
	if err := os.WriteFile(filepath.Join(profile, "settings.json"), []byte(`{"prompts":["/opt/piso/INFORMANT.md"],"defaultModel":"model-b"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := put(view.ID, ""); rec.Code != 200 {
		t.Fatalf("clear: status %d", rec.Code)
	}
	raw, _ = os.ReadFile(filepath.Join(profile, "settings.json"))
	if strings.Contains(string(raw), "defaultModel") || strings.Contains(string(raw), "defaultProvider") || !strings.Contains(string(raw), "INFORMANT.md") {
		t.Fatalf("settings after clear: %s", raw)
	}
}

// TestPutPiProviderSavesWhenCatalogFails is the regression guard: a provider
// whose model list cannot be fetched must still save and activate with a
// default model.
func TestPutPiProviderSavesWhenCatalogFails(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream detail that must not leak", http.StatusUnauthorized)
	}))
	defer fake.Close()
	s, profile := providerTestServer(t, fake)
	body := fmt.Sprintf(`{"name":"Broken","secretId":"11111111-1111-1111-1111-111111111111","baseUrl":%q,"defaultModel":"chosen-model"}`, fake.URL+"/v1")
	rec := httptest.NewRecorder()
	s.handlePutPiProvider(rec, httptest.NewRequest(http.MethodPut, "/api/v1/pi-provider", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "upstream detail") {
		t.Fatalf("leaked upstream detail: %s", rec.Body.String())
	}
	var view piProviderView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Applied {
		t.Fatalf("not applied: %+v", view)
	}
	raw, err := os.ReadFile(filepath.Join(profile, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"defaultModel": "chosen-model"`) {
		t.Fatalf("settings: %s", raw)
	}
}

func TestPiProviderStackSaveActivateDelete(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "pi-profile")
	t.Setenv("PISO_PI_PROFILE", profile)
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"One", "Two"} {
		if err := st.AddSecret(store.SecretRec{ID: name, Name: name, Placeholder: "piso_" + name, Value: "real-" + name}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{Store: st, DataDir: dir}
	add := func(name, secretID string) piProviderView {
		body := fmt.Sprintf(`{"name":%q,"secretId":%q,"baseUrl":"https://%s.example/v1"}`, name, secretID, name)
		rec := httptest.NewRecorder()
		s.handlePutPiProvider(rec, httptest.NewRequest(http.MethodPut, "/api/v1/pi-provider", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("add %s: %d %s", name, rec.Code, rec.Body.String())
		}
		var view piProviderView
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	one := add("One", "One")
	two := add("Two", "Two")
	if one.ID == two.ID {
		t.Fatalf("ids collided: %s", one.ID)
	}
	if st.ActivePiProviderID() != one.ID {
		t.Fatalf("first add must be active, got %s", st.ActivePiProviderID())
	}
	// Activate the second.
	rec := httptest.NewRecorder()
	s.handleActivatePiProvider(rec, httptest.NewRequest(http.MethodPost, "/api/v1/pi-provider/activate", strings.NewReader(fmt.Sprintf(`{"id":%q}`, two.ID))))
	if rec.Code != 200 {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body.String())
	}
	if st.ActivePiProviderID() != two.ID {
		t.Fatalf("active: %s", st.ActivePiProviderID())
	}
	raw, _ := os.ReadFile(filepath.Join(profile, "models.json"))
	if !strings.Contains(string(raw), `"`+one.ID+`"`) || !strings.Contains(string(raw), `"`+two.ID+`"`) {
		t.Fatalf("both providers must be written: %s", raw)
	}
	// Delete the active provider; the remaining one takes over.
	rec = httptest.NewRecorder()
	s.handleDeletePiProvider(rec, httptest.NewRequest(http.MethodPost, "/api/v1/pi-provider/delete", strings.NewReader(fmt.Sprintf(`{"id":%q}`, two.ID))))
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if st.ActivePiProviderID() != one.ID {
		t.Fatalf("fallback active: %s", st.ActivePiProviderID())
	}
	raw, _ = os.ReadFile(filepath.Join(profile, "models.json"))
	if strings.Contains(string(raw), `"`+two.ID+`"`) {
		t.Fatalf("deleted provider still present: %s", raw)
	}
}

func TestPiProviderModelsUnavailable(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret upstream detail", http.StatusUnauthorized)
	}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	s.Store.SetPiProvider(&store.PiProviderRec{SecretID: "11111111-1111-1111-1111-111111111111", BaseURL: fake.URL + "/v1"})
	rec := httptest.NewRecorder()
	s.handleGetPiProviderModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pi-provider/models", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret upstream detail") {
		t.Fatalf("response forwarded upstream detail: %s", rec.Body.String())
	}
	var catalog piProviderCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Catalog != "failed" || catalog.Error == "" {
		t.Fatalf("catalog: %+v", catalog)
	}
	if got := s.Store.PiProvider(); got == nil || got.CatalogError == "" {
		t.Fatalf("failure not cached: %+v", got)
	}
}

func TestPutPiProviderRejectsBadURL(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/pi-provider", strings.NewReader(`{"secretId":"x","baseUrl":"not a url"}`))
	rec := httptest.NewRecorder()
	s.handlePutPiProvider(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status %d", rec.Code)
	}
}

// TestPiProviderModelsFromUnsavedForm is the regression guard for loading a
// catalog before the provider has ever been saved: the add dialog passes the
// typed base URL and chosen secret directly.
func TestPiProviderModelsFromUnsavedForm(t *testing.T) {
	var gotAuth string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"data":[{"id":"model-a"},{"id":"model-b"}]}`)
	}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	if s.Store.PiProvider() != nil {
		t.Fatal("expected no saved provider")
	}
	q := url.Values{}
	q.Set("baseUrl", fake.URL+"/v1")
	q.Set("secretId", "11111111-1111-1111-1111-111111111111")
	rec := httptest.NewRecorder()
	s.handleGetPiProviderModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pi-provider/models?"+q.Encode(), nil))
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var catalog piProviderCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if strings.Join(catalog.Models, ",") != "model-a,model-b" || catalog.Catalog != "ok" {
		t.Fatalf("catalog: %+v", catalog)
	}
	if gotAuth != "Bearer sk-test-FAKE-not-written" {
		t.Fatalf("auth header %q", gotAuth)
	}
}

// TestPiProviderModelsChangedURLKeepsStoredCatalog guards the edit dialog:
// previewing a different endpoint must not overwrite the saved provider's
// catalog, which only describes the saved URL.
func TestPiProviderModelsChangedURLKeepsStoredCatalog(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"new-model"}]}`)
	}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	if err := s.Store.SetPiProvider(&store.PiProviderRec{
		ID:       "piso-one",
		SecretID: "11111111-1111-1111-1111-111111111111",
		BaseURL:  fake.URL + "/v1",
		Models:   []string{"old-model"},
	}); err != nil {
		t.Fatal(err)
	}
	q := url.Values{}
	q.Set("id", "piso-one")
	q.Set("baseUrl", fake.URL+"/v2")
	q.Set("secretId", "11111111-1111-1111-1111-111111111111")
	rec := httptest.NewRecorder()
	s.handleGetPiProviderModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pi-provider/models?"+q.Encode(), nil))
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	got := s.Store.PiProviderByID("piso-one")
	if got == nil || strings.Join(got.Models, ",") != "old-model" {
		t.Fatalf("stored catalog overwritten by a different URL: %+v", got)
	}
}

// TestPutPiProviderStoresPickerModels makes the card show the same loaded
// value the modal offered instead of reporting a pending catalog.
func TestPutPiProviderStoresPickerModels(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	body := fmt.Sprintf(`{"name":"AIML","secretId":"11111111-1111-1111-1111-111111111111","baseUrl":%q,"defaultModel":"model-b","models":["model-a","model-b","model-a","  "]}`, fake.URL+"/v1")
	rec := httptest.NewRecorder()
	s.handlePutPiProvider(rec, httptest.NewRequest(http.MethodPut, "/api/v1/pi-provider", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var view piProviderView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if strings.Join(view.Models, ",") != "model-a,model-b" || view.Catalog != "ok" {
		t.Fatalf("view: %+v", view)
	}
	if view.CheckedAt == "" {
		t.Fatalf("picker models were not stamped: %+v", view)
	}
}

// TestPiProviderCheckedEmptyCatalogIsKnown keeps a provider that answered
// with no models off the "model list pending" path: the card should say the
// list is empty, not that it is still loading.
func TestPiProviderCheckedEmptyCatalogIsKnown(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	if err := s.Store.SetPiProvider(&store.PiProviderRec{
		ID:       "piso-one",
		SecretID: "11111111-1111-1111-1111-111111111111",
		BaseURL:  fake.URL + "/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetPiProviderCatalog("piso-one", nil, ""); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleGetPiProvider(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pi-provider", nil))
	var list piProviderListView
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, p := range list.Providers {
		if p.ID != "piso-one" {
			continue
		}
		if p.Catalog != "ok" || p.CheckedAt == "" {
			t.Fatalf("checked-empty catalog reported as %q: %+v", p.Catalog, p)
		}
		return
	}
	t.Fatal("seeded provider missing from the list")
}

// TestPiProviderListFlatDefaultMatchesActive is the CLI contract guard: `piso
// up` reads defaultProvider/defaultModel from the top level of this response
// to write the worker profile. They must describe the activated profile and
// carry the same effectiveModel fallback the worker resolves against.
func TestPiProviderListFlatDefaultMatchesActive(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "pi-profile")
	t.Setenv("PISO_PI_PROFILE", profile)
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{ID: "sec", Name: "A", Placeholder: "piso_a", Value: "real-a"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, DataDir: dir}

	put := func(id, model string) piProviderView {
		body := fmt.Sprintf(`{"id":%q,"name":"A","secretId":"sec","baseUrl":"https://a.example/v1","defaultModel":%q,"models":["model-a","model-b"]}`, id, model)
		rec := httptest.NewRecorder()
		s.handlePutPiProvider(rec, httptest.NewRequest(http.MethodPut, "/api/v1/pi-provider", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
		}
		var view piProviderView
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	list := func() (piProviderListView, map[string]any) {
		rec := httptest.NewRecorder()
		s.handleGetPiProvider(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pi-provider", nil))
		if rec.Code != 200 {
			t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
		}
		var view piProviderListView
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		return view, raw
	}

	// No model chosen: the effective model is the first catalog entry.
	added := put("", "")
	view, raw := list()
	if view.ActiveID != added.ID || view.DefaultProvider != added.ID {
		t.Fatalf("flat provider %q, active %q, want %q", view.DefaultProvider, view.ActiveID, added.ID)
	}
	if view.DefaultModel != "model-a" || raw["defaultModel"] != "model-a" {
		t.Fatalf("flat model %v (view %q), want the catalog fallback model-a", raw["defaultModel"], view.DefaultModel)
	}

	// An explicit choice wins over the fallback.
	put(added.ID, "model-b")
	view, raw = list()
	if view.DefaultModel != "model-b" || raw["defaultModel"] != "model-b" || view.DefaultProvider != added.ID {
		t.Fatalf("flat default after save: %v / %q", raw["defaultModel"], view.DefaultProvider)
	}
	// defaultModel is always present, even when empty: absence is how a client
	// detects a gateway that predates the field, and that must not be confused
	// with an explicit clear.
	if _, ok := raw["defaultModel"]; !ok {
		t.Fatalf("defaultModel missing from the list response: %v", raw)
	}
}

// A tinfoil provider's catalog is served by a proxy that runs beside pi in the
// worker, so this gateway cannot reach it by design. It must report the reason
// instead of recording a failure, and must not clobber a cached list.
func TestPiProviderModelsLocalSidecar(t *testing.T) {
	// The sidecar must never be dialled, so the fake fails loudly if it is.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("gateway dialled a worker-sidecar URL: %s", r.URL)
	}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	s.Store.SetPiProvider(&store.PiProviderRec{
		SecretID: "11111111-1111-1111-1111-111111111111",
		BaseURL:  tinfoilProxyBaseURL,
		Kind:     piProviderKindTinfoil,
		Models:   []string{"kimi-k2", "llama-3.3-70b"},
	})
	rec := httptest.NewRecorder()
	s.handleGetPiProviderModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pi-provider/models", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var catalog piProviderCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Catalog != "local" {
		t.Fatalf("catalog state: %+v", catalog)
	}
	if catalog.Error != "" {
		t.Fatalf("local sidecar reported as an error: %+v", catalog)
	}
	if len(catalog.Models) != 2 {
		t.Fatalf("cached models dropped: %+v", catalog)
	}
	if got := s.Store.PiProvider(); got == nil || got.CatalogError != "" {
		t.Fatalf("local sidecar must not record a catalog failure: %+v", got)
	}
}

// Kind, not the URL, decides whether the gateway may fetch. A remote provider
// that happens to sit on the sidecar's address is still fetched, which is the
// property the URL-comparison version could not express.
func TestPiProviderModelsRemoteOnLoopbackIsFetched(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"on-prem-model"}]}`)
	}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	s.Store.SetPiProvider(&store.PiProviderRec{
		SecretID: "11111111-1111-1111-1111-111111111111",
		BaseURL:  fake.URL + "/v1",
		Kind:     piProviderKindRemote,
	})
	rec := httptest.NewRecorder()
	s.handleGetPiProviderModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pi-provider/models", nil))
	var catalog piProviderCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Catalog != "ok" || len(catalog.Models) != 1 || catalog.Models[0] != "on-prem-model" {
		t.Fatalf("remote provider must be fetched: %+v", catalog)
	}
}

// The tinfoil kind owns its URL: the dialog hides the field, so a client that
// sends something else must not be able to point the provider elsewhere.
func TestPiProviderEndpoint(t *testing.T) {
	if gotKind, gotURL, err := piProviderEndpoint(piProviderKindTinfoil, "https://evil.example.com/v1"); err != nil {
		t.Fatalf("tinfoil: %v", err)
	} else if gotKind != piProviderKindTinfoil || gotURL != tinfoilProxyBaseURL {
		t.Fatalf("tinfoil must pin its URL, got %q %q", gotKind, gotURL)
	}
	if _, gotURL, err := piProviderEndpoint(piProviderKindRemote, "https://api.example.com/v1"); err != nil {
		t.Fatalf("remote: %v", err)
	} else if gotURL != "https://api.example.com/v1" {
		t.Fatalf("remote must keep the operator URL, got %q", gotURL)
	}
	if _, _, err := piProviderEndpoint("bogus", "https://api.example.com/v1"); err == nil {
		t.Fatal("unknown kind must be rejected")
	}
}

// putPiProvider posts one provider body and returns the recorder.
func putPiProvider(t *testing.T, s *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/pi-provider", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	s.handlePutPiProvider(rec, req)
	return rec
}

// The tinfoil tab has no URL field, so the server pins it. A client that sends
// its own URL must not be able to redirect the provider.
func TestPutPiProviderTinfoilPinsURL(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	rec := putPiProvider(t, s, map[string]any{
		"name":     "Tinfoil",
		"secretId": "sec_local",
		"kind":     piProviderKindTinfoil,
		"baseUrl":  "https://evil.example.com/v1",
	})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := s.Store.PiProvider()
	if got == nil {
		t.Fatal("provider not saved")
	}
	if got.Kind != piProviderKindTinfoil || got.BaseURL != tinfoilProxyBaseURL {
		t.Fatalf("tinfoil provider not pinned: kind=%q baseUrl=%q", got.Kind, got.BaseURL)
	}
	var view piProviderView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Kind != piProviderKindTinfoil {
		t.Fatalf("kind missing from the view: %+v", view)
	}
}

// The kind is not derived from the URL, so it has to survive an unrelated edit
// that does not mention it. Without this, editing a tinfoil provider would
// silently turn it back into a remote one and the dashboard would dial it.
func TestPutPiProviderKeepsKindAcrossEdits(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	first := putPiProvider(t, s, map[string]any{
		"id": "piso-tinfoil", "name": "Tinfoil", "secretId": "sec_local",
		"kind": piProviderKindTinfoil, "defaultModel": "kimi-k2",
	})
	if first.Code != 200 {
		t.Fatalf("create: %d %s", first.Code, first.Body.String())
	}
	// An edit that resends kind, the way the dialog does.
	if rec := putPiProvider(t, s, map[string]any{
		"id": "piso-tinfoil", "name": "Tinfoil renamed", "secretId": "sec_local",
		"kind": piProviderKindTinfoil,
	}); rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	got := s.Store.PiProviderByID("piso-tinfoil")
	if got == nil || got.Kind != piProviderKindTinfoil || got.BaseURL != tinfoilProxyBaseURL {
		t.Fatalf("kind lost on edit: %+v", got)
	}
	if got.Name != "Tinfoil renamed" {
		t.Fatalf("edit did not apply: %+v", got)
	}
}

// A provider's apiKey becomes a literal credential on the model host. Kinds
// that are not literal values, or that workers may not use, fail at request
// time with a confusing mint or rule error, so refuse the binding at save.
func TestPutPiProviderRejectsNonAPIKeySecrets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		secret store.SecretRec
		want   string
	}{
		{
			name: "github-app",
			secret: store.SecretRec{
				ID: "sec_gh", Name: "GitHub", Placeholder: "piso_githubapp",
				Kind: model.SecretKindGithubApp,
			},
			want: "must be a plain API key",
		},
		{
			name: "gateway-only",
			secret: store.SecretRec{
				ID: "sec_go", Name: "Vault", Placeholder: "piso_vault", GatewayOnly: true,
			},
			want: "must be usable by workers",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			defer fake.Close()
			s, _ := providerTestServer(t, fake)
			if err := s.Store.AddSecret(tc.secret); err != nil {
				t.Fatal(err)
			}
			rec := putPiProvider(t, s, map[string]any{
				"name": "Bad", "secretId": tc.secret.ID, "baseUrl": "https://api.example.com/v1",
			})
			if rec.Code != 400 {
				t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("error %q missing %q", rec.Body.String(), tc.want)
			}
			if got := s.Store.PiProvider(); got != nil {
				t.Fatalf("rejected provider was saved: %+v", got)
			}
		})
	}
}

// Unknown kinds are refused rather than silently stored as remote, so a typo in
// a hand-written request cannot downgrade a local provider into a fetched one.
func TestPutPiProviderRejectsUnknownKind(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	rec := putPiProvider(t, s, map[string]any{
		"name": "Typo", "secretId": "sec_local", "baseUrl": "https://api.example.com/v1", "kind": "tinfoill",
	})
	if rec.Code != 400 {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// A tinfoil provider's real request leaves the worker's proxy for a numbered
// router enclave, so a secret bound to the loopback address substitutes
// nothing and every call fails 407. Saving is still allowed (the binding is a
// separate record), but the operator has to be told.
func TestPutPiProviderWarnsWhenSecretMissesTheEnclave(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	// Replace the default secret with one bound to loopback, the mistake that
	// looks right because it matches the URL the provider dials.
	if err := s.Store.DeleteSecret("sec_local"); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AddSecret(store.SecretRec{
		ID: "sec_loop", Name: "Tinfoil", Placeholder: "piso_tinfoil",
		Value: "tk_fake", AllowedHosts: []string{"127.0.0.1"},
	}); err != nil {
		t.Fatal(err)
	}
	rec := putPiProvider(t, s, map[string]any{
		"name": "Tinfoil", "secretId": "sec_loop", "kind": piProviderKindTinfoil,
	})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var view piProviderView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Warning, "*.tinfoil.sh") {
		t.Fatalf("warning %q does not name the host the secret must allow", view.Warning)
	}
}

func TestSecretCoversTinfoilFleet(t *testing.T) {
	for _, tc := range []struct {
		hosts []string
		want  bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"127.0.0.1"}, false},
		{[]string{"tinfoil.sh"}, false},
		{[]string{"*.tinfoil.sh"}, true},
		{[]string{"*"}, true},
		{[]string{"api.example.com", "*.tinfoil.sh"}, true},
		// One numbered router works until the next attestation picks another.
		{[]string{"router-0.tinfoil.sh"}, false},
		// The rejected spellings never reach here, but if they did the rule set
		// is empty and nothing is substitutable.
		{[]string{"router-*.tinfoil.sh"}, false},
	} {
		if got := secretCoversTinfoilFleet(tc.hosts); got != tc.want {
			t.Errorf("secretCoversTinfoilFleet(%v) = %v, want %v", tc.hosts, got, tc.want)
		}
	}
}

// A local provider's catalog is only ever as fresh as its last save, because
// the list lives behind a proxy inside the worker. Reporting "pending" would
// claim the gateway is still asking when it never will, and the dashboard
// renders that verbatim on the provider card.
func TestProviderViewCatalogForLocalKind(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer fake.Close()
	s, _ := providerTestServer(t, fake)
	for _, tc := range []struct {
		name string
		rec  store.PiProviderRec
		want string
	}{
		{"remote never fetched", store.PiProviderRec{ID: "piso-a", SecretID: "sec_local"}, "pending"},
		{"remote with models", store.PiProviderRec{ID: "piso-b", SecretID: "sec_local", Models: []string{"m1"}}, "ok"},
		{"tinfoil with nothing cached", store.PiProviderRec{ID: "piso-c", SecretID: "sec_local", Kind: piProviderKindTinfoil}, "local"},
		{"tinfoil with a stale failure", store.PiProviderRec{ID: "piso-d", SecretID: "sec_local", Kind: piProviderKindTinfoil, CatalogError: "boom"}, "local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := s.providerViewFor(&tc.rec, profileFileState{})
			if view.Catalog != tc.want {
				t.Fatalf("catalog = %q, want %q", view.Catalog, tc.want)
			}
		})
	}
}

// A provider whose secret is not resolvable on this gateway but is already
// written into the shared profile is applied and serving traffic. It must not
// be reported as "not on this gateway yet", and a re-apply must not drop it
// from models.json.
func TestPiProviderKeepsAppliedProviderWhenSecretMissing(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "pi-profile")
	t.Setenv("PISO_PI_PROFILE", profile)
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{ID: "sec_local", Name: "AIML", Placeholder: "piso_aimlapi", Value: "sk-test-FAKE", TechnocoreID: "11111111-1111-1111-1111-111111111111"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, DataDir: dir}
	if err := st.SetPiProvider(&store.PiProviderRec{ID: "piso-one", Name: "AIML", SecretID: "11111111-1111-1111-1111-111111111111", BaseURL: "https://api.example.test/v1"}); err != nil {
		t.Fatal(err)
	}
	if list := s.applyPiProviders(); list.Warning != "" {
		t.Fatalf("unexpected warning on first apply: %q", list.Warning)
	}
	// The secret vanishes on this gateway (e.g. it is still syncing) while the
	// profile file keeps the placeholder that already works.
	if err := st.DeleteSecret("sec_local"); err != nil {
		t.Fatal(err)
	}
	view := s.piProviderViewByID("piso-one")
	if view == nil || !view.Applied || view.Warning != "" || view.Placeholder != "piso_aimlapi" {
		t.Fatalf("applied provider reported as missing: %+v", view)
	}
	if list := s.applyPiProviders(); list.Warning != "" {
		t.Fatalf("re-apply dropped a working provider: %q", list.Warning)
	}
	raw, err := os.ReadFile(filepath.Join(profile, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "piso_aimlapi") {
		t.Fatalf("working placeholder dropped from models.json: %s", raw)
	}
}

// A row saved before kind existed still points at the worker-side proxy. It
// must render as local, not "pending" or "unavailable", and the models
// endpoint must refuse to probe loopback for it.
func TestPiProviderLegacyTinfoilURLIsLocal(t *testing.T) {
	s, _ := providerTestServer(t, httptest.NewServer(http.NotFoundHandler()))
	legacy := &store.PiProviderRec{ID: "piso-legacy", SecretID: "sec_local", BaseURL: tinfoilProxyBaseURL}
	view := s.providerViewFor(legacy, profileFileState{})
	if view.Catalog != "local" {
		t.Fatalf("legacy tinfoil catalog = %q, want local", view.Catalog)
	}
	if err := s.Store.SetPiProvider(legacy); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleGetPiProviderModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pi-provider/models?id=piso-legacy", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"catalog":"local"`) {
		t.Fatalf("legacy tinfoil models probe: %d %s", rec.Code, rec.Body.String())
	}
}

// A cloud ship has no gateway on its host, so its /opt/piso/pi-profile mount is
// an empty directory and pi would start with no provider. The worker API hands
// it the same two files a colocated worker reads off disk.
func TestWorkerPiProfileServesMountedFiles(t *testing.T) {
	const real = "sk-test-FAKE-not-written"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"model-a"},{"id":"model-b"}]}`)
	}))
	defer fake.Close()
	s, profile := providerTestServer(t, fake)
	s.Store.SetPiProvider(&store.PiProviderRec{
		SecretID: "11111111-1111-1111-1111-111111111111",
		BaseURL:  "https://api.example.test/v1",
		Models:   []string{"model-a", "model-b"},
	})
	apply := httptest.NewRecorder()
	s.handleGetPiProvider(apply, httptest.NewRequest(http.MethodGet, "/api/v1/pi-provider", nil))
	if apply.Code != 200 {
		t.Fatalf("apply status %d body %s", apply.Code, apply.Body.String())
	}

	rec := httptest.NewRecorder()
	s.handleWorkerGetPiProfile(rec, httptest.NewRequest(http.MethodGet, "/api/v1/worker/pi-profile", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var got workerPiProfileView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), real) {
		t.Fatalf("response leaked the secret: %s", rec.Body.String())
	}
	// The payload must be the files themselves, byte for byte: a ship that got a
	// re-rendering of the store could drift from a colocated worker.
	onDisk, err := os.ReadFile(filepath.Join(profile, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Models != string(onDisk) {
		t.Fatalf("models.json served %q, on disk %q", got.Models, onDisk)
	}
	if !strings.Contains(got.Models, `"apiKey": "piso_aimlapi"`) {
		t.Fatalf("placeholder missing: %s", got.Models)
	}
	if !strings.Contains(got.Settings, `"defaultModel": "model-a"`) {
		t.Fatalf("default model missing from settings: %s", got.Settings)
	}
	if got.DefaultModel != "model-a" || !got.Applied {
		t.Fatalf("view: %+v", got)
	}
}

// An unapplied profile is a gateway that cannot say what the active model is;
// answering 200 with empty files would wipe a ship's working profile.
func TestWorkerPiProfileFailsWhenNothingApplied(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer fake.Close()
	s, profile := providerTestServer(t, fake)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	// applyPiProviders writes both files even with no provider, so break the
	// mount instead: a missing profile directory is the real failure.
	if err := os.RemoveAll(profile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleWorkerGetPiProfile(rec, httptest.NewRequest(http.MethodGet, "/api/v1/worker/pi-profile", nil))
	if rec.Code != 500 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"settings":"`) {
		t.Fatalf("must not serve a profile it could not read: %s", rec.Body.String())
	}
}

// A gateway that has never applied a profile (fresh install, or a state file
// restored without the profile directory) still has to answer: reconcile once
// from the store, then serve.
func TestWorkerPiProfileBootstrapsMissingProfile(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"model-a"}]}`)
	}))
	defer fake.Close()
	s, profile := providerTestServer(t, fake)
	s.Store.SetPiProvider(&store.PiProviderRec{
		SecretID: "11111111-1111-1111-1111-111111111111",
		BaseURL:  "https://api.example.test/v1",
		Models:   []string{"model-a"},
	})
	rec := httptest.NewRecorder()
	s.handleWorkerGetPiProfile(rec, httptest.NewRequest(http.MethodGet, "/api/v1/worker/pi-profile", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var got workerPiProfileView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Models, `"apiKey": "piso_aimlapi"`) || got.DefaultModel != "model-a" {
		t.Fatalf("bootstrap served %+v", got)
	}
	if _, err := os.Stat(filepath.Join(profile, "models.json")); err != nil {
		t.Fatalf("profile not written: %v", err)
	}
}
