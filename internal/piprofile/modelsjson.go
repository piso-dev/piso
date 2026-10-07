// Package piprofile writes the shared, secret-free pi models.json that workers
// and the monitor mount. The CLI profile sync and the gateway both call it.
package piprofile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ProviderName is the neutral namespace for the dashboard-managed endpoint.
const ProviderName = "piso"

// LegacyProviderName is read only during migration of older piso profiles.
const LegacyProviderName = "routstr"

// ProviderAPI is the pi models.json api adapter for an OpenAI-compatible URL.
const ProviderAPI = "openai-completions"

// WriteProviderModels writes providers.piso with a literal placeholder and
// a plain base URL. The models list is left empty: pi fills it from
// <baseUrl>/models. Real secret values must not be passed as apiKey.
func WriteProviderModels(dir, apiKey, baseURL string) error {
	if dir == "" {
		return os.ErrInvalid
	}
	apiKey = strings.TrimSpace(apiKey)
	baseURL = strings.TrimSpace(baseURL)
	if apiKey == "" || baseURL == "" {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "models.json")
	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(MigrateProviderModels(raw), &doc); err != nil {
			doc = map[string]any{}
		}
	}
	providers, _ := doc["providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	entry, _ := providers[ProviderName].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	entry["api"] = ProviderAPI
	entry["apiKey"] = apiKey
	entry["baseUrl"] = baseURL
	entry["models"] = []any{}
	providers[ProviderName] = entry
	doc["providers"] = providers
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// KeepDashboardModels reports whether dest already carries the shared
// provider the dashboard writes. A later piso up must not replace that file
// with the host agent's models.json.
func KeepDashboardModels(dest []byte) bool {
	if len(bytes.TrimSpace(dest)) == 0 {
		return false
	}
	var doc struct {
		Providers map[string]struct {
			APIKey  string `json:"apiKey"`
			BaseURL string `json:"baseUrl"`
		} `json:"providers"`
	}
	if json.Unmarshal(dest, &doc) != nil || doc.Providers == nil {
		return false
	}
	for _, name := range []string{ProviderName, LegacyProviderName} {
		p, ok := doc.Providers[name]
		if ok && strings.HasPrefix(strings.TrimSpace(p.APIKey), "piso_") && strings.TrimSpace(p.BaseURL) != "" {
			return true
		}
	}
	return false
}

// MigrateProviderModels removes the old piso-owned namespace without touching
// an independently configured provider or changing any credentials/endpoints.
// If both names exist, the current piso entry wins.
func MigrateProviderModels(raw []byte) []byte {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return raw
	}
	providers, _ := doc["providers"].(map[string]any)
	legacy, _ := providers[LegacyProviderName].(map[string]any)
	key, _ := legacy["apiKey"].(string)
	if !strings.HasPrefix(strings.TrimSpace(key), "piso_") {
		return raw
	}
	if _, exists := providers[ProviderName]; !exists {
		providers[ProviderName] = legacy
	}
	delete(providers, LegacyProviderName)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return raw
	}
	return append(out, '\n')
}
