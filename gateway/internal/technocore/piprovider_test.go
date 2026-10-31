package technocore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

func TestSyncPiProviderDefaultCompatibility(t *testing.T) {
	for _, tt := range []struct{ name, field, want string }{
		{"older coordinator omits field", "", "unbiased/pareto-26.10-preview"},
		{"explicit clear", `,"defaultModel":""`, ""},
		{"new selection", `,"defaultModel":"new-model"`, "new-model"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := testStore(t)
			dir := t.TempDir()
			t.Setenv("PISO_PI_PROFILE", filepath.Join(dir, "pi-profile"))
			if err := st.AddSecret(store.SecretRec{ID: "secret", Placeholder: "piso_test", Value: "fake"}); err != nil {
				t.Fatal(err)
			}
			if err := st.SetPiProvider(&store.PiProviderRec{SecretID: "secret", BaseURL: "https://example.test/v1", DefaultModel: "unbiased/pareto-26.10-preview", UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"metadata":{"secretId":"secret","baseUrl":"https://example.test/v1"%s},"updated_at":"2026-01-02T00:00:00Z"}`, tt.field)
			}))
			defer srv.Close()
			old := serverHTTP
			serverHTTP = srv.Client()
			defer func() { serverHTTP = old }()
			id, err := tc.Generate(srv.URL, "test")
			if err != nil {
				t.Fatal(err)
			}
			if err := syncPiProvider(st, dir, id); err != nil {
				t.Fatal(err)
			}
			if got := st.PiProvider().DefaultModel; got != tt.want {
				t.Fatalf("saved default = %q, want %q", got, tt.want)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "pi-profile", "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := json.Unmarshal(raw, &settings); err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if _, ok := settings["defaultModel"]; ok {
					t.Fatalf("default not cleared: %s", raw)
				}
			} else if settings["defaultModel"] != tt.want {
				t.Fatalf("worker default: %s", raw)
			}
		})
	}
}

func TestSyncPiProviderStack(t *testing.T) {
	st := testStore(t)
	dir := t.TempDir()
	t.Setenv("PISO_PI_PROFILE", filepath.Join(dir, "pi-profile"))
	if err := st.AddSecret(store.SecretRec{ID: "sec1", Placeholder: "piso_one", Value: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{ID: "sec2", Placeholder: "piso_two", Value: "fake"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"metadata":{"secretId":"sec2","baseUrl":"https://two.test/v1","defaultModel":"m2","activeId":"piso-two","providers":[{"id":"piso-one","name":"One","secretId":"sec1","baseUrl":"https://one.test/v1","defaultModel":"m1"},{"id":"piso-two","name":"Two","secretId":"sec2","baseUrl":"https://two.test/v1","defaultModel":"m2"}]},"updated_at":"2026-01-02T00:00:00Z"}`)
	}))
	defer srv.Close()
	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()
	id, err := tc.Generate(srv.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := syncPiProvider(st, dir, id); err != nil {
		t.Fatal(err)
	}
	if stack := st.PiProviderStack(); len(stack) != 2 {
		t.Fatalf("stack: %+v", stack)
	}
	if got := st.ActivePiProviderID(); got != "piso-two" {
		t.Fatalf("active: %q", got)
	}
	models, err := os.ReadFile(filepath.Join(dir, "pi-profile", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(models), `"piso-one"`) || !strings.Contains(string(models), `"piso-two"`) {
		t.Fatalf("models.json missing providers: %s", models)
	}
	settings, err := os.ReadFile(filepath.Join(dir, "pi-profile", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(settings), `"defaultProvider": "piso-two"`) || !strings.Contains(string(settings), `"defaultModel": "m2"`) {
		t.Fatalf("settings: %s", settings)
	}
}

func TestPublishPiProviderSendsStack(t *testing.T) {
	st := testStore(t)
	if err := st.AddSecret(store.SecretRec{ID: "sec1", Placeholder: "piso_one", Value: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{ID: "sec2", Placeholder: "piso_two", Value: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPiProvider(store.PiProviderRec{ID: "piso-one", Name: "One", SecretID: "sec1", BaseURL: "https://one.test/v1", DefaultModel: "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPiProviderActive(store.PiProviderRec{ID: "piso-two", Name: "Two", SecretID: "sec2", BaseURL: "https://two.test/v1", DefaultModel: "m2"}); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()
	id, err := tc.Generate(srv.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := publishPiProvider(st, id); err != nil {
		t.Fatal(err)
	}
	meta, _ := body["metadata"].(map[string]any)
	if meta["activeId"] != "piso-two" {
		t.Fatalf("activeId: %+v", meta)
	}
	providers, _ := meta["providers"].([]any)
	if len(providers) != 2 {
		t.Fatalf("providers: %+v", meta["providers"])
	}
	first, _ := providers[0].(map[string]any)
	if first["id"] != "piso-one" || first["secretId"] != "sec1" {
		t.Fatalf("first provider: %+v", first)
	}
	// Kind decides whether a gateway may fetch a catalog, so a synced profile
	// that loses it would be probed by every peer. It must travel.
	if _, ok := first["kind"]; !ok {
		t.Fatalf("kind missing from the published provider: %+v", first)
	}
}

// A kind that survives the round trip: published, then merged back by a peer.
func TestPiProviderKindRoundTrip(t *testing.T) {
	st := testStore(t)
	if err := st.AddSecret(store.SecretRec{ID: "sec1", Placeholder: "piso_tinfoil", Value: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPiProviderActive(store.PiProviderRec{ID: "piso-tinfoil", Name: "Tinfoil", SecretID: "sec1", BaseURL: "http://127.0.0.1:3301/v1", Kind: "tinfoil"}); err != nil {
		t.Fatal(err)
	}
	var published map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&published)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()
	id, err := tc.Generate(srv.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := publishPiProvider(st, id); err != nil {
		t.Fatal(err)
	}
	// Serve that document back to a fresh gateway, as a coordinator would.
	raw, err := json.Marshal(published)
	if err != nil {
		t.Fatal(err)
	}
	peer := testStore(t)
	peerDir := t.TempDir()
	t.Setenv("PISO_PI_PROFILE", filepath.Join(peerDir, "pi-profile"))
	if err := peer.AddSecret(store.SecretRec{ID: "sec1", Placeholder: "piso_tinfoil", Value: "fake"}); err != nil {
		t.Fatal(err)
	}
	read := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	}))
	defer read.Close()
	serverHTTP = read.Client()
	readID, err := tc.Generate(read.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := syncPiProvider(peer, peerDir, readID); err != nil {
		t.Fatal(err)
	}
	got := peer.PiProviderByID("piso-tinfoil")
	if got == nil || got.Kind != "tinfoil" || got.BaseURL != "http://127.0.0.1:3301/v1" {
		t.Fatalf("kind lost in sync: %+v", got)
	}
}

// syncFrom serves one coordinator document to a gateway and returns the store
// afterwards. Each call gets a fresh identity so a test can replay several
// documents in a row, which is what a reconcile does on a timer.
func syncFrom(t *testing.T, st *store.Store, dir, doc string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, doc)
	}))
	defer srv.Close()
	old := serverHTTP
	serverHTTP = srv.Client()
	defer func() { serverHTTP = old }()
	id, err := tc.Generate(srv.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := syncPiProvider(st, dir, id); err != nil {
		t.Fatal(err)
	}
}

// The approved list is account configuration: it must survive the coordinator,
// both ways, or a paired gateway silently narrows every provider back to its
// default model on the next reconcile.
func TestPiProviderApprovedModelsRoundTrip(t *testing.T) {
	st := testStore(t)
	if err := st.AddSecret(store.SecretRec{ID: "sec1", Placeholder: "piso_one", Value: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPiProviderActive(store.PiProviderRec{
		ID:             "piso-one",
		Name:           "One",
		SecretID:       "sec1",
		BaseURL:        "https://one.test/v1",
		DefaultModel:   "m1",
		APIType:        "anthropic-messages",
		ApprovedModels: []string{"m1", "m2"},
		Models:         []string{"m1", "m2", "m3"},
	}); err != nil {
		t.Fatal(err)
	}
	// Publish: the list is in the payload, and an unset list is sent as [] so a
	// clear can travel (null would read as "omitted").
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	old := serverHTTP
	serverHTTP = srv.Client()
	pubID, err := tc.Generate(srv.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := publishPiProvider(st, pubID); err != nil {
		t.Fatal(err)
	}
	serverHTTP = old
	meta, _ := body["metadata"].(map[string]any)
	providers, _ := meta["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("providers: %+v", meta["providers"])
	}
	first, _ := providers[0].(map[string]any)
	if got, _ := first["approvedModels"].([]any); len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Fatalf("published approvedModels: %+v", first["approvedModels"])
	}
	if first["api"] != "anthropic-messages" {
		t.Fatalf("published api: %+v", first["api"])
	}
	// The catalog is fetched per gateway and is not account configuration.
	if _, ok := first["models"]; ok {
		t.Fatalf("catalog should not be published: %+v", first)
	}

	// A fresh gateway that receives that document ends up with the same policy.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	peer := testStore(t)
	peerDir := t.TempDir()
	t.Setenv("PISO_PI_PROFILE", filepath.Join(peerDir, "pi-profile"))
	if err := peer.AddSecret(store.SecretRec{ID: "sec1", Placeholder: "piso_one", Value: "fake"}); err != nil {
		t.Fatal(err)
	}
	syncFrom(t, peer, peerDir, string(raw))
	got := peer.PiProviderByID("piso-one")
	if got == nil {
		t.Fatal("provider not synced")
	}
	if strings.Join(got.ApprovedModels, ",") != "m1,m2" {
		t.Fatalf("approvedModels lost in sync: %+v", got)
	}
	if got.APIType != "anthropic-messages" {
		t.Fatalf("api lost in sync: %+v", got)
	}
}

// An older coordinator omits approvedModels, kind, and api. Absence is not a
// clear: the local record keeps its policy instead of being narrowed to the
// default model on every reconcile.
func TestSyncPiProviderPreservesLocalPolicy(t *testing.T) {
	st := testStore(t)
	dir := t.TempDir()
	t.Setenv("PISO_PI_PROFILE", filepath.Join(dir, "pi-profile"))
	if err := st.AddSecret(store.SecretRec{ID: "sec1", Placeholder: "piso_one", Value: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPiProviderActive(store.PiProviderRec{
		ID:             "piso-one",
		Name:           "One",
		SecretID:       "sec1",
		BaseURL:        "https://one.test/v1",
		Kind:           "tinfoil",
		APIType:        "anthropic-messages",
		DefaultModel:   "m1",
		ApprovedModels: []string{"m1", "m2"},
		// Older than the document below, so the sync pulls instead of
		// publishing the local copy back.
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	syncFrom(t, st, dir, `{"metadata":{"activeId":"piso-one","providers":[{"id":"piso-one","name":"One","secretId":"sec1","baseUrl":"https://one.test/v1","defaultModel":"m1"}]},"updated_at":"2026-01-02T00:00:00Z"}`)
	if want := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC); !st.PiProviderByID("piso-one").UpdatedAt.Equal(want) {
		t.Fatalf("the document was not merged: %+v", st.PiProviderByID("piso-one"))
	}
	got := st.PiProviderByID("piso-one")
	if got == nil {
		t.Fatal("provider missing after sync")
	}
	if strings.Join(got.ApprovedModels, ",") != "m1,m2" {
		t.Fatalf("sync without the field erased the list: %+v", got)
	}
	if got.Kind != "tinfoil" || got.APIType != "anthropic-messages" {
		t.Fatalf("sync without the field erased the transport: %+v", got)
	}
}

// An explicit empty list is a decision — "only the default model" — and it
// travels, so clearing it on one gateway clears it on the rest.
func TestSyncPiProviderAppliesExplicitEmptyList(t *testing.T) {
	st := testStore(t)
	dir := t.TempDir()
	t.Setenv("PISO_PI_PROFILE", filepath.Join(dir, "pi-profile"))
	if err := st.AddSecret(store.SecretRec{ID: "sec1", Placeholder: "piso_one", Value: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPiProviderActive(store.PiProviderRec{
		ID:             "piso-one",
		Name:           "One",
		SecretID:       "sec1",
		BaseURL:        "https://one.test/v1",
		DefaultModel:   "m1",
		ApprovedModels: []string{"m1", "m2"},
		UpdatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	syncFrom(t, st, dir, `{"metadata":{"activeId":"piso-one","providers":[{"id":"piso-one","name":"One","secretId":"sec1","baseUrl":"https://one.test/v1","defaultModel":"m1","approvedModels":[]}]},"updated_at":"2026-01-02T00:00:00Z"}`)
	got := st.PiProviderByID("piso-one")
	if got == nil {
		t.Fatal("provider missing after sync")
	}
	if len(got.ApprovedModels) != 0 {
		t.Fatalf("explicit clear ignored: %+v", got)
	}
	if strings.Join(got.AllowedModelIDs(), ",") != "m1" {
		t.Fatalf("allowed set: %+v", got.AllowedModelIDs())
	}
}
