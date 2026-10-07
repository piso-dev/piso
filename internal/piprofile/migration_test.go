package piprofile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateProviderModels(t *testing.T) {
	for _, tt := range []struct {
		name, raw, wantKey string
		legacy             bool
	}{
		{"old managed profile", `{"providers":{"routstr":{"apiKey":"piso_aimlapi","baseUrl":"https://api.example.test/v1","models":[{"id":"saved"}]},"other":{"apiKey":"keep"}},"extra":true}`, "piso_aimlapi", false},
		{"new entry wins", `{"providers":{"piso":{"apiKey":"piso_new"},"routstr":{"apiKey":"piso_old"}}}`, "piso_new", false},
		{"independent provider retained", `{"providers":{"routstr":{"apiKey":"$USER_KEY"}}}`, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := MigrateProviderModels([]byte(tt.raw))
			if string(MigrateProviderModels(raw)) != string(raw) {
				t.Fatal("migration is not idempotent")
			}
			var doc struct {
				Providers map[string]struct {
					APIKey string `json:"apiKey"`
				} `json:"providers"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			if got := doc.Providers[ProviderName].APIKey; got != tt.wantKey {
				t.Fatalf("key = %q", got)
			}
			if _, ok := doc.Providers[LegacyProviderName]; ok != tt.legacy {
				t.Fatalf("legacy entry presence = %v", ok)
			}
		})
	}
}

func TestWriteProviderMigratesOldNamespaceAndDefault(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"providers":{"routstr":{"apiKey":"piso_aimlapi","baseUrl":"https://api.example.test/v1"},"other":{"apiKey":"keep"}}}`)
	if err := os.WriteFile(filepath.Join(dir, "models.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"defaultProvider":"routstr","defaultModel":"chosen","theme":"dark"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteProviderModels(dir, "piso_aimlapi", "https://api.example.test/v1"); err != nil {
		t.Fatal(err)
	}
	if err := WriteDefaultModel(dir, "chosen"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var models struct {
		Providers map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(raw, &models); err != nil {
		t.Fatal(err)
	}
	if models.Providers[ProviderName] == nil || models.Providers["other"] == nil || models.Providers[LegacyProviderName] != nil {
		t.Fatalf("providers: %s", raw)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["defaultProvider"] != "piso" || settings["defaultModel"] != "chosen" || settings["theme"] != "dark" {
		t.Fatalf("settings: %s", raw)
	}
	if !KeepDashboardModels([]byte(`{"providers":{"piso":{"apiKey":"piso_x","baseUrl":"https://example.test/v1"}}}`)) {
		t.Fatal("new namespace not protected")
	}
}
