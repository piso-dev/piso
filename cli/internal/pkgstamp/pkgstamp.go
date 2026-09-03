// Package pkgstamp decides whether the worker npm tree needs a reinstall.
package pkgstamp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// DepsFromHostPackageJSON prefers pinned versions from the host lock-ish
// package.json when present.
func DepsFromHostPackageJSON(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil, err
	}
	return pkg.Dependencies, nil
}

// DepsFromPackages turns settings.json "npm:foo" entries into name → "*".
func DepsFromPackages(packages []string) map[string]string {
	out := map[string]string{}
	for _, p := range packages {
		name := strings.TrimSpace(p)
		name = strings.TrimPrefix(name, "npm:")
		if name == "" {
			continue
		}
		out[name] = "*"
	}
	return out
}

// MergeDeps prefers host versions, then fills missing names from packages.
func MergeDeps(host map[string]string, fromSettings map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range fromSettings {
		out[k] = v
	}
	for k, v := range host {
		out[k] = v
	}
	return out
}

// Hash is a stable digest of the dependency map.
func Hash(deps map[string]string) string {
	keys := make([]string, 0, len(deps))
	for k := range deps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(deps[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// PackageJSON renders a private package.json for the worker npm dir.
func PackageJSON(deps map[string]string) ([]byte, error) {
	if deps == nil {
		deps = map[string]string{}
	}
	return json.MarshalIndent(map[string]any{
		"name":         "pi-extensions",
		"private":      true,
		"dependencies": deps,
	}, "", "  ")
}
