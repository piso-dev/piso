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
	if !KeepDashboardModels([]byte(`{"providers":{"piso-openrouter":{"apiKey":"piso_x","baseUrl":"https://x"}}}`)) {
		t.Fatal("stack provider must keep the dashboard profile")
	}
	if KeepDashboardModels(nil) {
		t.Fatal("empty dest must not block host sync")
	}
}

func TestWriteProviderStackReconcilesManagedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	orig := []byte(`{"providers":{"other":{"apiKey":"keep"},"piso":{"apiKey":"piso_old","baseUrl":"https://old"},"piso-stale":{"apiKey":"piso_stale","baseUrl":"https://stale"}}}`)
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	err := WriteProviderStack(dir, []Provider{
		{ID: "piso-one", Name: "One", APIKey: "piso_one", BaseURL: "https://one/v1", Models: []string{"m1", "m2"}},
		{ID: "piso-two", APIKey: "piso_two", BaseURL: "https://two/v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)
	// The legacy "piso" key is managed, so it is removed when not in the stack.
	for _, gone := range []string{`"piso":`, `piso-stale`, "piso_old", "piso_stale"} {
		if strings.Contains(got, gone) {
			t.Fatalf("stale managed key %q survived: %s", gone, got)
		}
	}
	for _, want := range []string{`"other"`, `"apiKey": "keep"`, `"piso-one"`, `"piso-two"`, `"id": "m1"`, `"id": "m2"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	// An empty stack clears every managed key but keeps others.
	if err := WriteProviderStack(dir, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	got = string(raw)
	if strings.Contains(got, "piso-one") || !strings.Contains(got, `"other"`) {
		t.Fatalf("empty stack reconcile: %s", got)
	}
}

func TestWithProviderDefault(t *testing.T) {
	out := WithProviderDefault([]byte(`{"prompts":["/x"]}`), "piso-two", "m1")
	got := string(out)
	if !strings.Contains(got, `"defaultProvider": "piso-two"`) || !strings.Contains(got, `"defaultModel": "m1"`) || !strings.Contains(got, "prompts") {
		t.Fatalf("apply: %s", got)
	}
	cleared := string(WithProviderDefault(out, "piso-two", ""))
	if strings.Contains(cleared, "defaultProvider") || strings.Contains(cleared, "defaultModel") {
		t.Fatalf("clear: %s", cleared)
	}
}
