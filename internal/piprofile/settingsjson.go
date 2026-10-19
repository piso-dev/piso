package piprofile

import (
	"bytes"
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
//
// Prefer WithProviderDefault: a multi-provider dashboard stack uses
// "piso-<id>" keys, and pinning the default to ProviderName names a provider
// that may not exist in models.json.
func WithDefaultModel(settings []byte, modelID string) []byte {
	return WithProviderDefault(settings, ProviderName, modelID)
}

// CanonicalProviderID maps the legacy provider namespace onto the current one.
// It matches what the worker extension does when it registers a provider, so a
// settings.json default keeps resolving after a dashboard migration.
func CanonicalProviderID(id string) string {
	id = strings.TrimSpace(id)
	if id == LegacyProviderName {
		return ProviderName
	}
	return id
}

// dashboardOwnedKeys are the settings keys that pick a startup model. They
// belong to the shared profile, not to the agent directory of whatever
// machine happened to run `piso up`: inheriting them makes a worker start on
// the operator's personal model, and enabledModels outranks defaultModel in
// pi's startup resolution, so it silently wins over the dashboard choice.
var dashboardOwnedKeys = []string{"defaultProvider", "defaultModel", "enabledModels"}

// workerLocalKeys are settings pi writes back on the host that a worker must
// not inherit through the read-only shared profile. lastChangelogVersion is
// the important one: pi advances it after rendering "What's New", but a
// worker's ~/.pi/agent/settings.json is a symlink into the read-only profile
// mount, so the advance is lost. A stale host value (e.g. the host's pi is on
// an older release than the worker image's pinned pi) then replays the
// changelog in every new worker session.
var workerLocalKeys = []string{"lastChangelogVersion"}

// DropChangelogVersion removes the worker-local markers (currently
// lastChangelogVersion) from settings JSON, preserving every other key. It
// returns the input untouched when the JSON is unparseable or no key is
// present, so it is safe to call on an already-clean profile.
func DropChangelogVersion(settings []byte) []byte {
	var m map[string]any
	if json.Unmarshal(settings, &m) != nil || m == nil {
		return settings
	}
	changed := false
	for _, key := range workerLocalKeys {
		if _, ok := m[key]; ok {
			delete(m, key)
			changed = true
		}
	}
	if !changed {
		return settings
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return settings
	}
	return append(out, '\n')
}

// AdoptHostSettings merges the host settings a worker may inherit (theme,
// tools, prompts, packages, ...) with the dashboard-owned selection already in
// dest. Once dest names a dashboard provider, every selection key comes from
// dest and the host's equivalents are dropped, including enabledModels, which
// outranks defaultModel in pi's startup resolution. While dest names no
// dashboard provider, the host settings are used as before: there is no
// dashboard choice for them to override.
//
// Whatever branch wins, pi's host-local changelog marker is dropped: the
// worker profile is read-only and must never carry the host's last-seen
// version.
func AdoptHostSettings(host, dest []byte) []byte {
	var base map[string]any
	if len(bytes.TrimSpace(host)) == 0 || json.Unmarshal(host, &base) != nil || base == nil {
		// No usable host profile: the shared profile is already authoritative.
		return DropChangelogVersion(dest)
	}
	var saved map[string]any
	_ = json.Unmarshal(dest, &saved)
	if !dashboardManagedProvider(saved) {
		return DropChangelogVersion(host)
	}
	for _, key := range dashboardOwnedKeys {
		if v, ok := saved[key]; ok {
			base[key] = v
		} else {
			delete(base, key)
		}
	}
	for _, key := range workerLocalKeys {
		delete(base, key)
	}
	out, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		return dest
	}
	return append(out, '\n')
}

// dashboardManagedProvider reports whether settings name a provider in the
// dashboard namespace. The model may legitimately be empty: the dashboard
// saves a provider before its catalog resolves, and the worker then starts on
// the catalog's first entry.
func dashboardManagedProvider(m map[string]any) bool {
	if m == nil {
		return false
	}
	provider, _ := m["defaultProvider"].(string)
	return ManagedProviderKey(provider)
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
