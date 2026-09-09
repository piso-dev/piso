package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

func TestProviderEnvKeyAndHost(t *testing.T) {
	if got := providerEnvKey("routstr"); got != "ROUTSTR_API_KEY" {
		t.Fatalf("got %q", got)
	}
	if got := importHost("https://routstr.example.test/v1"); got != "routstr.example.test" {
		t.Fatalf("got %q", got)
	}
}

func TestImportPiKeysUpsertNoValueInResult(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "workers", "w1"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st}
	const fake = "sk-test-FAKE-KEY-not-a-real-secret"
	first, err := s.importPiKeys([]PiKeyProvider{{Name: "routstr", APIKey: fake, Host: "https://api.example.test/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Placeholder == "" || first[0].EnvKey != "ROUTSTR_API_KEY" {
		t.Fatalf("first: %+v", first)
	}
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), fake) || strings.Contains(string(raw), "apiKey") {
		t.Fatalf("result leaked key: %s", raw)
	}
	second, err := s.importPiKeys([]PiKeyProvider{{Name: "routstr", APIKey: fake, Host: "https://api.example.test/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Placeholder != first[0].Placeholder {
		t.Fatalf("placeholder changed: %s vs %s", first[0].Placeholder, second[0].Placeholder)
	}
	third, err := s.importPiKeys([]PiKeyProvider{{Name: "routstr", APIKey: fake + "-rotated", Host: "https://api.example.test/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if third[0].Placeholder != first[0].Placeholder {
		t.Fatal("rotation must keep placeholder")
	}
	got, ok := st.SecretByEnvKey("ROUTSTR_API_KEY")
	if !ok || got.Value != fake+"-rotated" {
		t.Fatalf("value not updated: %+v", got)
	}
	env, err := os.ReadFile(filepath.Join(dir, "workers", "w1", store.PlaceholdersEnvName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), fake) {
		t.Fatal("env file leaked value")
	}
	if !strings.Contains(string(env), "ROUTSTR_API_KEY="+first[0].Placeholder) {
		t.Fatalf("env: %s", env)
	}
}
