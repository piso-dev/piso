package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
