package piprofile

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func TestCanonicalProviderID(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"routstr", "piso"},
		{" piso ", "piso"},
		{"piso-ai-ml-api", "piso-ai-ml-api"},
		{"", ""},
	} {
		if got := CanonicalProviderID(tt.in); got != tt.want {
			t.Fatalf("CanonicalProviderID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A worker must not start on the operator's personal model: once the shared
// profile names a dashboard provider, the host's selection keys are dropped
// and enabledModels (which outranks defaultModel in pi) never leaks in.
func TestAdoptHostSettingsDropsHostModelSelection(t *testing.T) {
	host := []byte(`{
		"theme": "dark",
		"tools": ["read"],
		"defaultProvider": "anthropic",
		"defaultModel": "claude-opus-4-8",
		"enabledModels": ["anthropic/*", "openai/*"]
	}`)
	dest := []byte(`{"defaultProvider":"piso-ai-ml-api","defaultModel":"deepseek/deepseek-v4.1-flash"}`)

	got := decode(t, AdoptHostSettings(host, dest))
	if got["theme"] != "dark" {
		t.Fatalf("host settings lost: %v", got)
	}
	if got["defaultProvider"] != "piso-ai-ml-api" || got["defaultModel"] != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("dashboard selection lost: %v", got)
	}
	if _, ok := got["enabledModels"]; ok {
		t.Fatalf("host enabledModels leaked into the worker profile: %v", got)
	}
}

// A provider saved before its catalog resolves carries no model. The host's
// model must still not fill that gap: the dashboard choice is "first catalog
// entry", not "whatever the operator last ran on their laptop".
func TestAdoptHostSettingsDropsHostModelForCataloguelessProvider(t *testing.T) {
	host := []byte(`{"theme":"dark","defaultProvider":"anthropic","defaultModel":"claude-opus-4-8","enabledModels":["anthropic/*"]}`)
	dest := []byte(`{"defaultProvider":"piso-two"}`)

	got := decode(t, AdoptHostSettings(host, dest))
	if got["defaultProvider"] != "piso-two" {
		t.Fatalf("dashboard provider lost: %v", got)
	}
	for _, key := range []string{"defaultModel", "enabledModels"} {
		if _, ok := got[key]; ok {
			t.Fatalf("host %s leaked into the worker profile: %v", key, got)
		}
	}
	if got["theme"] != "dark" {
		t.Fatalf("host settings lost: %v", got)
	}
}

// With no dashboard provider yet there is nothing to override, so a machine
// whose operator configured pi keeps its inherited default (previous behavior).
func TestAdoptHostSettingsInheritsWhenNoDashboardSelection(t *testing.T) {
	host := []byte(`{"defaultProvider":"anthropic","defaultModel":"claude-opus-4-8"}`)
	dest := []byte(`{"prompts":["/opt/piso/INFORMANT.md"]}`)

	got := decode(t, AdoptHostSettings(host, dest))
	if got["defaultProvider"] != "anthropic" || got["defaultModel"] != "claude-opus-4-8" {
		t.Fatalf("host default not inherited: %v", got)
	}
}

// No host agent directory: the shared profile is used verbatim.
func TestAdoptHostSettingsWithoutHostProfile(t *testing.T) {
	dest := []byte(`{"theme":"light","defaultProvider":"piso-two","defaultModel":"m2"}`)
	for _, host := range [][]byte{nil, {}, []byte("  \n"), []byte("{not json")} {
		if got := string(AdoptHostSettings(host, dest)); got != string(dest) {
			t.Fatalf("host %q: got %s", host, got)
		}
	}
}

// WithProviderDefault must be usable for a stack provider id; WithDefaultModel
// stays pinned to the legacy namespace.
func TestWithProviderDefaultWritesStackProvider(t *testing.T) {
	got := decode(t, WithProviderDefault([]byte(`{"theme":"dark"}`), "piso-ai-ml-api", "deepseek/deepseek-v4.1-flash"))
	if got["defaultProvider"] != "piso-ai-ml-api" || got["defaultModel"] != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("provider default: %v", got)
	}
	if got["theme"] != "dark" {
		t.Fatalf("other keys lost: %v", got)
	}
	cleared := decode(t, WithProviderDefault([]byte(`{"defaultProvider":"piso","defaultModel":"m1","theme":"dark"}`), "", "m1"))
	if _, ok := cleared["defaultProvider"]; ok {
		t.Fatalf("clear kept provider: %v", cleared)
	}
	if _, ok := cleared["defaultModel"]; ok {
		t.Fatalf("clear kept model: %v", cleared)
	}
}
