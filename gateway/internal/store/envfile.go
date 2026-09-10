package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// PlaceholdersEnvName is the worker-safe env file next to state.json.
const PlaceholdersEnvName = "placeholders.env"

// PlaceholdersEnvPath is $PISO_DATA/placeholders.env given the state file path.
func PlaceholdersEnvPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), PlaceholdersEnvName)
}

// DeriveEnvKey derives a POSIX env var name from a human secret name:
// uppercase; keep [A-Z0-9_], map every other rune to '_'; trim edge '_';
// prefix '_' if it would start with a digit. Empty result returns "" so the
// caller can ask for an explicit key.
func DeriveEnvKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return ""
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "_" + out
	}
	return out
}

// ValidEnvKey reports a POSIX-ish exported name: [A-Za-z_][A-Za-z0-9_]*.
func ValidEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if i == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// SecretVisibleToWorker reports whether rec should appear in slug's env file.
// Empty Workers or a "*" entry means all workers (the default).
func SecretVisibleToWorker(rec SecretRec, slug string) bool {
	if rec.EnvKey == "" || rec.Placeholder == "" {
		return false
	}
	if len(rec.Workers) == 0 {
		return true
	}
	for _, w := range rec.Workers {
		if w == "" || w == "*" || strings.EqualFold(w, slug) {
			return true
		}
	}
	return false
}

// RenderPlaceholdersEnvForWorker writes KEY=piso_… lines visible to slug.
func RenderPlaceholdersEnvForWorker(secrets []SecretRec, slug string) string {
	var filtered []SecretRec
	for _, rec := range secrets {
		if SecretVisibleToWorker(rec, slug) {
			filtered = append(filtered, rec)
		}
	}
	return RenderPlaceholdersEnv(filtered)
}

// WorkersDir is $PISO_DATA/workers next to state.json.
func WorkersDir(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "workers")
}

// WorkerPlaceholdersEnvPath is $PISO_DATA/workers/<slug>/placeholders.env.
func WorkerPlaceholdersEnvPath(statePath, slug string) string {
	return filepath.Join(WorkersDir(statePath), slug, PlaceholdersEnvName)
}

// ListWorkerSlugs returns directory names under workers/.
func ListWorkerSlugs(statePath string) ([]string, error) {
	dir := WorkersDir(statePath)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && e.Name() != "" && e.Name() != "." && e.Name() != ".." {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// WriteWorkerPlaceholdersEnv writes one slug's env file (placeholders only).
func WriteWorkerPlaceholdersEnv(statePath, slug string, secrets []SecretRec) error {
	path := WorkerPlaceholdersEnvPath(statePath, slug)
	body := RenderPlaceholdersEnvForWorker(secrets, slug)
	if strings.Contains(body, "\x00") {
		return fmt.Errorf("placeholders env: invalid content")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteAllWorkerEnvs rewrites every workers/<slug>/placeholders.env.
func WriteAllWorkerEnvs(statePath string, secrets []SecretRec) error {
	slugs, err := ListWorkerSlugs(statePath)
	if err != nil {
		return err
	}
	for _, slug := range slugs {
		if err := WriteWorkerPlaceholdersEnv(statePath, slug, secrets); err != nil {
			return err
		}
	}
	return nil
}

// RenderPlaceholdersEnv writes KEY=piso_… lines. Real secret values are never
// included. Duplicate env keys keep the first secret (stable by EnvKey sort).
func RenderPlaceholdersEnv(secrets []SecretRec) string {
	type pair struct {
		key string
		val string
	}
	seen := map[string]bool{}
	var rows []pair
	for _, rec := range secrets {
		if rec.EnvKey == "" || rec.Placeholder == "" {
			continue
		}
		if !ValidEnvKey(rec.EnvKey) {
			continue
		}
		if seen[rec.EnvKey] {
			continue
		}
		seen[rec.EnvKey] = true
		rows = append(rows, pair{key: rec.EnvKey, val: rec.Placeholder})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	var b strings.Builder
	b.WriteString("# piso placeholders only — never real secrets. Sourced by piso attach.\n")
	for _, row := range rows {
		b.WriteString(row.key)
		b.WriteString("=")
		b.WriteString(row.val)
		b.WriteString("\n")
	}
	return b.String()
}

// WritePlaceholdersEnv writes the worker env file beside state.json at 0600.
func WritePlaceholdersEnv(statePath string, secrets []SecretRec) error {
	path := PlaceholdersEnvPath(statePath)
	body := RenderPlaceholdersEnv(secrets)
	if strings.Contains(body, "\x00") {
		return fmt.Errorf("placeholders env: invalid content")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) writePlaceholdersEnvLocked() error {
	return WriteAllWorkerEnvs(s.path, s.state.Secrets)
}
