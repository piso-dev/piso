package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSyncPiProfilePreservesDefault(t *testing.T) {
	for _, tt := range []struct {
		name, body, want string
		status           int
		legacy           bool
	}{
		{"gateway unavailable", `{}`, "unbiased/pareto-26.10-preview", 503, false},
		{"older gateway", `{"secretId":"test"}`, "unbiased/pareto-26.10-preview", 200, false},
		{"explicit clear", `{"defaultModel":""}`, "", 200, false},
		{"new default", `{"defaultModel":"new-model"}`, "new-model", 200, false},
		{"legacy profile without gateway", `{}`, "unbiased/pareto-26.10-preview", 503, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			agent := filepath.Join(dir, "agent")
			dest := filepath.Join(dir, "pi-profile")
			t.Setenv("PISO_DATA", dir)
			t.Setenv("PI_AGENT_DIR", agent)
			for _, p := range []string{agent, dest} {
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string]string{
				filepath.Join(agent, "settings.json"): `{"theme":"test"}`,
				filepath.Join(dest, "settings.json"):  `{"defaultProvider":"piso","defaultModel":"unbiased/pareto-26.10-preview"}`,
				filepath.Join(dest, "models.json"):    `{"providers":{"piso":{"apiKey":"piso_test","baseUrl":"https://example.test/v1"}}}`,
			}
			if tt.legacy {
				files[filepath.Join(dest, "settings.json")] = `{"defaultProvider":"routstr","defaultModel":"unbiased/pareto-26.10-preview"}`
				files[filepath.Join(dest, "models.json")] = `{"providers":{"routstr":{"apiKey":"piso_test","baseUrl":"https://example.test/v1"}}}`
			}
			for p, body := range files {
				if err := os.WriteFile(p, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tt.status); w.Write([]byte(tt.body)) }))
			defer srv.Close()
			t.Setenv("PISO_GATEWAY", srv.URL)
			if err := syncPiProfile(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(dest, "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := json.Unmarshal(raw, &settings); err != nil {
				t.Fatal(err)
			}
			if settings["theme"] != "test" {
				t.Fatalf("host settings lost: %s", raw)
			}
			if tt.want == "" {
				if _, ok := settings["defaultModel"]; ok {
					t.Fatalf("not cleared: %s", raw)
				}
			} else if settings["defaultModel"] != tt.want || settings["defaultProvider"] != "piso" {
				t.Fatalf("worker default: %s", raw)
			}
			raw, err = os.ReadFile(filepath.Join(dest, "models.json"))
			if err != nil {
				t.Fatal(err)
			}
			var models struct {
				Providers map[string]any `json:"providers"`
			}
			if err := json.Unmarshal(raw, &models); err != nil {
				t.Fatal(err)
			}
			if models.Providers["piso"] == nil || models.Providers["routstr"] != nil {
				t.Fatalf("provider namespace not migrated: %s", raw)
			}
		})
	}
}

// syncFixture lays out a host agent dir and a shared profile, points
// PISO_DATA/PI_AGENT_DIR at them, and serves the given gateway handler.
func syncFixture(t *testing.T, gateway http.HandlerFunc, hostSettings, destSettings, destModels string) string {
	t.Helper()
	dir := t.TempDir()
	agent := filepath.Join(dir, "agent")
	dest := filepath.Join(dir, "pi-profile")
	t.Setenv("PISO_DATA", dir)
	t.Setenv("PI_AGENT_DIR", agent)
	for _, p := range []string{agent, dest} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(agent, "settings.json"): hostSettings,
		filepath.Join(dest, "settings.json"):  destSettings,
		filepath.Join(dest, "models.json"):    destModels,
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(gateway)
	t.Cleanup(srv.Close)
	t.Setenv("PISO_GATEWAY", srv.URL)
	if err := syncPiProfile(); err != nil {
		t.Fatal(err)
	}
	return dest
}

func readProfileSettings(t *testing.T, dest string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dest, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings %s: %v", raw, err)
	}
	return settings
}

// The regression: a host pi install used to donate its model selection to the
// worker profile on every `piso up`, so the dashboard default never survived.
func TestSyncPiProfileIgnoresHostModelSelection(t *testing.T) {
	const active = `{"providers":[{"id":"piso-ai-ml-api","defaultModel":"deepseek/deepseek-v4.1-flash","effectiveModel":"deepseek/deepseek-v4.1-flash","active":true}],"activeId":"piso-ai-ml-api","defaultProvider":"piso-ai-ml-api","defaultModel":"deepseek/deepseek-v4.1-flash","applied":true}`
	dest := syncFixture(t,
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(active)) },
		`{"theme":"dark","defaultProvider":"anthropic","defaultModel":"claude-opus-4-8","enabledModels":["anthropic/*","openai/*"]}`,
		`{"defaultProvider":"piso-ai-ml-api","defaultModel":"stale-model"}`,
		`{"providers":{"piso-ai-ml-api":{"apiKey":"piso_aimlapi","baseUrl":"https://api.aimlapi.com/v1"}}}`,
	)
	got := readProfileSettings(t, dest)
	if got["defaultProvider"] != "piso-ai-ml-api" || got["defaultModel"] != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("dashboard default not applied: %v", got)
	}
	if _, ok := got["enabledModels"]; ok {
		t.Fatalf("host enabledModels leaked into the worker profile: %v", got)
	}
	if got["theme"] != "dark" {
		t.Fatalf("host settings not adopted: %v", got)
	}
	if got["prompts"] == nil {
		t.Fatalf("informant prompt missing: %v", got)
	}
}

// A provider saved before its catalog resolved has no model of its own; the
// stack view's effectiveModel is what the worker will start on.
func TestSyncPiProfileUsesEffectiveModel(t *testing.T) {
	const active = `{"providers":[{"id":"piso-two","defaultModel":"","effectiveModel":"model-a","active":true}],"activeId":"piso-two","defaultProvider":"piso-two","defaultModel":"model-a","applied":true}`
	dest := syncFixture(t,
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(active)) },
		`{"theme":"dark"}`,
		`{"defaultProvider":"piso-two","defaultModel":"model-a"}`,
		`{"providers":{"piso-two":{"apiKey":"piso_two","baseUrl":"https://two.example/v1"}}}`,
	)
	got := readProfileSettings(t, dest)
	if got["defaultProvider"] != "piso-two" || got["defaultModel"] != "model-a" {
		t.Fatalf("effective model not applied: %v", got)
	}
}

// Gateway down: the shared profile's stack provider survives. It used to be
// dropped because only "piso"/"routstr" were accepted.
func TestSyncPiProfileKeepsStackProviderWhenGatewayDown(t *testing.T) {
	dest := syncFixture(t,
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) },
		`{"theme":"dark"}`,
		`{"defaultProvider":"piso-two","defaultModel":"m2"}`,
		`{"providers":{"piso-two":{"apiKey":"piso_two","baseUrl":"https://two.example/v1"}}}`,
	)
	got := readProfileSettings(t, dest)
	if got["defaultProvider"] != "piso-two" || got["defaultModel"] != "m2" {
		t.Fatalf("stack provider dropped while the gateway was down: %v", got)
	}
}

// A legacy profile keeps resolving after the namespace migration.
func TestSyncPiProfileMigratesLegacyProviderDefault(t *testing.T) {
	dest := syncFixture(t,
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) },
		`{"theme":"dark"}`,
		`{"defaultProvider":"routstr","defaultModel":"m1"}`,
		`{"providers":{"routstr":{"apiKey":"piso_legacy","baseUrl":"https://legacy.example/v1"}}}`,
	)
	got := readProfileSettings(t, dest)
	if got["defaultProvider"] != "piso" || got["defaultModel"] != "m1" {
		t.Fatalf("legacy default not migrated: %v", got)
	}
}
