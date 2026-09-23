package piprofile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureModels = `{
  "providers": {
    "routstr": {
      "baseUrl": "https://example.test/v1",
      "api": "openai-completions",
      "apiKey": "sk-test-FAKE-KEY-do-not-use",
      "models": [{"id": "demo"}]
    }
  }
}`

func TestExtractAndSanitize(t *testing.T) {
	keys, err := ExtractProviderKeys([]byte(fixtureModels))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "routstr" || keys[0].APIKey != "sk-test-FAKE-KEY-do-not-use" {
		t.Fatalf("extract: %+v", keys)
	}
	out, err := SanitizeModelsJSON([]byte(fixtureModels), map[string]string{"routstr": "piso_routstr_abc"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "sk-test-FAKE-KEY-do-not-use") {
		t.Fatalf("real key remained:\n%s", out)
	}
	if !strings.Contains(string(out), "piso_routstr_abc") {
		t.Fatalf("placeholder missing:\n%s", out)
	}
}

func TestSanitizeEmptiesUnknownProviderKey(t *testing.T) {
	out, err := SanitizeModelsJSON([]byte(fixtureModels), map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "sk-test-FAKE-KEY-do-not-use") {
		t.Fatal("unknown provider kept real key")
	}
}

func TestReadSettingsPackages(t *testing.T) {
	pkgs, err := ReadSettingsPackages([]byte(`{"packages":["npm:pi-browser","npm:pi-hermes-memory"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 || pkgs[0] != "npm:pi-browser" {
		t.Fatalf("%v", pkgs)
	}
}

func TestEnsureInformantPromptsAddsOnce(t *testing.T) {
	out1 := EnsureInformantPrompts([]byte("{}\n"))
	if !strings.Contains(string(out1), informantPromptPath) {
		t.Fatalf("missing prompt: %s", out1)
	}
	// second call does not duplicate
	out2 := EnsureInformantPrompts(out1)
	if strings.Count(string(out2), informantPromptPath) != 1 {
		t.Fatalf("duplicated: %s", out2)
	}
}

func TestEnsureInformantPromptsPreservesOtherSettings(t *testing.T) {
	in := []byte(`{"theme":"dark","prompts":["/custom/a.md"]}`)
	out := EnsureInformantPrompts(in)
	s := string(out)
	if !strings.Contains(s, "dark") || !strings.Contains(s, "/custom/a.md") || !strings.Contains(s, informantPromptPath) {
		t.Fatalf("clobbered: %s", s)
	}
}

func TestEnsureInformantPromptsIncludesMonitorPrompt(t *testing.T) {
	out := EnsureInformantPrompts([]byte("{}\n"))
	s := string(out)
	if !strings.Contains(s, informantPromptPath) || !strings.Contains(s, monitorPromptPath) {
		t.Fatalf("missing prompt(s): %s", s)
	}
	// adding both once, twice is idempotent
	out2 := EnsureInformantPrompts(out)
	if strings.Count(string(out2), informantPromptPath) != 1 || strings.Count(string(out2), monitorPromptPath) != 1 {
		t.Fatalf("duplicated: %s", out2)
	}
}

func TestEnsureInformantPromptsHandlesEmptyAndExistingStrings(t *testing.T) {
	if !strings.Contains(string(EnsureInformantPrompts(nil)), informantPromptPath) {
		t.Fatal("nil settings should still yield the prompt")
	}
	// []string prompts form
	out := EnsureInformantPrompts([]byte(`{"prompts":["/a.md"]}`))
	if strings.Count(string(out), informantPromptPath) != 1 {
		t.Fatalf("[]string form: %s", out)
	}
}

func TestHostOrDestPrefersHost(t *testing.T) {
	src, fromHost := HostOrDest([]byte(`{"providers":{}}`), nil, []byte(`keep-me`))
	if !fromHost || !strings.Contains(string(src), "providers") {
		t.Fatalf("fromHost=%v src=%s", fromHost, src)
	}
}

func TestHostOrDestKeepsDestWhenHostMissing(t *testing.T) {
	src, fromHost := HostOrDest(nil, errors.New("no such file"), []byte(`keep-me`))
	if fromHost || string(src) != "keep-me" {
		t.Fatalf("fromHost=%v src=%s", fromHost, src)
	}
	src, fromHost = HostOrDest([]byte("  \n"), nil, []byte(`keep-me`))
	if fromHost || string(src) != "keep-me" {
		t.Fatalf("empty host: fromHost=%v src=%s", fromHost, src)
	}
}

func TestWriteProfileDoesNotClobberExistingModels(t *testing.T) {
	dir := t.TempDir()
	want := []byte("{\n  \"providers\": {\"routstr\": {\"apiKey\": \"piso_x\"}}\n}\n")
	if err := os.WriteFile(filepath.Join(dir, "models.json"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteProfile(dir, []byte(`{"theme":"dark"}`), nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("clobbered models.json:\n%s", got)
	}
}
