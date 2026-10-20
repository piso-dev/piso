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
