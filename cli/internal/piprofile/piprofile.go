// Package piprofile reads the host pi agent dir and writes a secret-free
// profile for workers. Real API keys never appear in the written files.
package piprofile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AgentDir is ~/.pi/agent, or PI_AGENT_DIR when set (tests).
func AgentDir() (string, error) {
	if v := strings.TrimSpace(os.Getenv("PI_AGENT_DIR")); v != "" {
		return filepath.Abs(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pi", "agent"), nil
}

// ProfileDir is $PISO_DATA/pi-profile.
func ProfileDir(dataDir string) string {
	return filepath.Join(dataDir, "pi-profile")
}

// ModelsPath is the host models.json (may contain real apiKey values).
func ModelsPath(agentDir string) string {
	return filepath.Join(agentDir, "models.json")
}

// SettingsPath is the host settings.json.
func SettingsPath(agentDir string) string {
	return filepath.Join(agentDir, "settings.json")
}

// HostPackageJSON is ~/.pi/agent/npm/package.json when present.
func HostPackageJSON(agentDir string) string {
	return filepath.Join(agentDir, "npm", "package.json")
}

// ProviderKey is one extractable apiKey from models.json.
type ProviderKey struct {
	Name   string `json:"name"`
	APIKey string `json:"apiKey"`
	Host   string `json:"host"`
}

type modelsFile struct {
	Providers map[string]json.RawMessage `json:"providers"`
}

type providerFields struct {
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey"`
}

// ExtractProviderKeys reads host models.json. The returned APIKey values
// must be sent only to the gateway import API, never written to disk.
func ExtractProviderKeys(raw []byte) ([]ProviderKey, error) {
	var file modelsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("models.json: %w", err)
	}
	var out []ProviderKey
	for name, blob := range file.Providers {
		var p providerFields
		if err := json.Unmarshal(blob, &p); err != nil {
			continue
		}
		key := strings.TrimSpace(p.APIKey)
		if key == "" {
			continue
		}
		out = append(out, ProviderKey{Name: name, APIKey: key, Host: p.BaseURL})
	}
	return out, nil
}

// SanitizeModelsJSON replaces providers.*.apiKey with placeholders[name].
// Unknown providers keep an empty apiKey. Real keys never remain.
func SanitizeModelsJSON(raw []byte, placeholders map[string]string) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("models.json: %w", err)
	}
	provRaw, ok := root["providers"]
	if !ok {
		return json.Marshal(map[string]any{"providers": map[string]any{}})
	}
	var providers map[string]map[string]any
	if err := json.Unmarshal(provRaw, &providers); err != nil {
		return nil, fmt.Errorf("models.json providers: %w", err)
	}
	for name, p := range providers {
		if p == nil {
			p = map[string]any{}
		}
		if ph, ok := placeholders[name]; ok && ph != "" {
			p["apiKey"] = ph
		} else {
			p["apiKey"] = ""
		}
		providers[name] = p
	}
	encoded, err := json.Marshal(providers)
	if err != nil {
		return nil, err
	}
	root["providers"] = encoded
	out, err := json.MarshalIndent(map[string]json.RawMessage(root), "", "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')
	return out, nil
}

// WriteProfile writes settings.json and sanitized models.json under dest.
func WriteProfile(dest string, settings, models []byte) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	if len(settings) == 0 {
		settings = []byte("{}\n")
	}
	if len(models) == 0 {
		models = []byte("{\n  \"providers\": {}\n}\n")
	}
	if err := os.WriteFile(filepath.Join(dest, "settings.json"), settings, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, "models.json"), models, 0o600)
}

// ReadSettingsPackages returns settings.json "packages" (npm:name entries).
func ReadSettingsPackages(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return s.Packages, nil
}

// informantPromptPath is the in-image prompt template teaching pi to emit
// semantic activity events (Tier B). monitorPromptPath is the project-manager
// (PM) template. Both are loaded via settings.json "prompts": every worker's
// pi reads INFORMANT (it narrates its own work); the monitor additionally
// reads MONITOR (it curates + pokes). PISO_ROLE=monitor + the MONITOR content
// give the monitor the PM frame.
const (
	informantPromptPath = "/opt/piso/INFORMANT.md"
	monitorPromptPath   = "/opt/piso/MONITOR.md"
)

// EnsureInformantPrompts returns a copy of settings with the informant (and,
// for monitors, the monitor) prompt template(s) added to the "prompts" array
// (deduped, preserving every other key). Empty/nil settings both produce a
// valid `{"prompts": [...]}`.
func EnsureInformantPrompts(settings []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(settings, &m); err != nil || m == nil {
		m = map[string]any{}
	}
	// read existing prompts (string or []string)
	has := func(path string) bool {
		switch v := m["prompts"].(type) {
		case []any:
			for _, e := range v {
				if s, ok := e.(string); ok && s == path {
					return true
				}
			}
		case []string:
			for _, s := range v {
				if s == path {
					return true
				}
			}
		case nil:
		default:
		}
		return false
	}
	add := func(path string) {
		switch v := m["prompts"].(type) {
		case []any:
			m["prompts"] = append(v, path)
		case []string:
			m["prompts"] = append(v, path)
		case nil:
			m["prompts"] = []any{path}
		default:
			m["prompts"] = []any{path}
		}
	}
	if !has(informantPromptPath) {
		add(informantPromptPath)
	}
	if !has(monitorPromptPath) {
		add(monitorPromptPath)
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return settings // fall back to verbatim on marshal failure
	}
	return append(out, '\n')
}
