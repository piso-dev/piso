package piprofile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteProviderModelsEmptyListAndPlaceholderOnly(t *testing.T) {
	dir := t.TempDir()
	if err := WriteProviderModels(dir, "piso_aimlapi", "https://api.example.test/v1"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{`"api": "openai-completions"`, `"apiKey": "piso_aimlapi"`, `"baseUrl": "https://api.example.test/v1"`, `"models": []`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "sk-") {
		t.Fatalf("real-looking key in profile: %s", got)
	}
}

func TestWriteProviderModelsPreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	orig := []byte("{\n  \"theme\": \"dark\",\n  \"providers\": {\"other\": {\"apiKey\": \"keep\"}}\n}\n")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteProviderModels(dir, "piso_x", "https://api.example.test/v1"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, `"theme": "dark"`) || !strings.Contains(got, `"other"`) {
		t.Fatalf("clobbered other keys: %s", got)
	}
	if !strings.Contains(got, `"apiKey": "keep"`) {
		t.Fatalf("other provider key lost: %s", got)
	}
}

func TestKeepDashboardModels(t *testing.T) {
	if !KeepDashboardModels([]byte(`{"providers":{"routstr":{"apiKey":"piso_x","baseUrl":"https://x"}}}`)) {
		t.Fatal("expected keep")
	}
	if KeepDashboardModels([]byte(`{"providers":{"routstr":{"apiKey":"piso_x"}}}`)) {
		t.Fatal("url-less profile must not block host sync")
	}
	if KeepDashboardModels(nil) {
		t.Fatal("empty dest must not block host sync")
	}
}
