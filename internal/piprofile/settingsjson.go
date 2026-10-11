package piprofile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// WithDefaultModel returns settings JSON with the dashboard-owned default
// model applied under the legacy piso provider, preserving every other key.
// An empty modelID removes both keys so pi falls back to first-available
// selection. Unreadable settings are treated as empty rather than failing the
// caller.
func WithDefaultModel(settings []byte, modelID string) []byte {
	return WithProviderDefault(settings, ProviderName, modelID)
}

// WithProviderDefault returns settings JSON with defaultProvider/defaultModel
// set to providerID/modelID, preserving every other key. An empty providerID
// or modelID removes both keys so pi falls back to first-available selection.
func WithProviderDefault(settings []byte, providerID, modelID string) []byte {
	var m map[string]any
	if json.Unmarshal(settings, &m) != nil || m == nil {
		m = map[string]any{}
	}
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if providerID == "" || modelID == "" {
		delete(m, "defaultProvider")
		delete(m, "defaultModel")
	} else {
		m["defaultProvider"] = providerID
		m["defaultModel"] = modelID
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return settings
	}
	return append(out, '\n')
}

// WriteDefaultModel applies WithDefaultModel to the profile's settings.json.
// A missing file is created; every key other than the default is preserved.
func WriteDefaultModel(dir, modelID string) error {
	return WriteProviderDefault(dir, ProviderName, modelID)
}

// WriteProviderDefault applies WithProviderDefault to settings.json. A missing
// file is created; every key other than the default is preserved.
func WriteProviderDefault(dir, providerID, modelID string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, WithProviderDefault(raw, providerID, modelID), 0o600)
}
