package piprofile

import (
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
