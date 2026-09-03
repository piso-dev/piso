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
