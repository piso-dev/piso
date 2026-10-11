package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"piso/gateway/internal/store"
)

func TestPiProviderAppliedRequiresMatchingDefault(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer fake.Close()
	s, profile := providerTestServer(t, fake)
	if err := s.Store.SetPiProvider(&store.PiProviderRec{SecretID: "11111111-1111-1111-1111-111111111111", BaseURL: fake.URL + "/v1", DefaultModel: "unbiased/pareto-26.10-preview"}); err != nil {
		t.Fatal(err)
	}
	if view := s.applyPiProviders(); !view.Applied {
		t.Fatalf("not applied: %+v", view)
	}
	if err := os.WriteFile(filepath.Join(profile, "settings.json"), []byte(`{"prompts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if view := s.piProviderListView(); view.Applied || view.Warning == "" {
		t.Fatalf("missing default falsely reported applied: %+v", view)
	}
	if view := s.applyPiProviders(); !view.Applied {
		t.Fatalf("not repaired: %+v", view)
	}
}

func TestPiProviderViewExplicitEmptyDefault(t *testing.T) {
	raw, err := json.Marshal(piProviderView{})
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if value, ok := view["defaultModel"]; !ok || value != "" {
		t.Fatalf("clear must differ from a legacy response: %s", raw)
	}
}

func TestPiProviderEffectiveModelFallsBackToCatalog(t *testing.T) {
	// No default, but a cached catalog: the first model becomes the effective
	// default so a session has something concrete to start on.
	if got := effectivePiModel(&store.PiProviderRec{Models: []string{"", "first", "second"}}); got != "first" {
		t.Fatalf("effective: %q", got)
	}
	if got := effectivePiModel(&store.PiProviderRec{DefaultModel: "explicit", Models: []string{"first"}}); got != "explicit" {
		t.Fatalf("explicit must win: %q", got)
	}
	if got := effectivePiModel(&store.PiProviderRec{}); got != "" {
		t.Fatalf("empty must stay empty: %q", got)
	}
}
