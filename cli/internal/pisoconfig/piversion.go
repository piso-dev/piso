package pisoconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The pi version pinned into the worker image. Persisted in the data dir
// (~/.piso/pi-version) so `piso up` does not clobber a value written by
// `piso update` (which rewrites only the staged Dockerfile). The staged
// Dockerfile is re-copied from the repo on every `piso up`, so the pin must
// live here and be re-injected at stage time.
//
// Default matches the worker Dockerfile's ARG (DefaultPiVersion). Keep in sync
// with worker/Dockerfile.
const DefaultPiVersion = "0.84.4"

// PiVersionPath is ~/.piso/pi-version.
func PiVersionPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pi-version"), nil
}

// LoadPiVersion returns the persisted pin, or DefaultPiVersion when absent.
func LoadPiVersion() string {
	path, err := PiVersionPath()
	if err != nil {
		return DefaultPiVersion
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return DefaultPiVersion
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return DefaultPiVersion
	}
	return v
}

// SavePiVersion persists the pin (best-effort: a failure leaves the previous
// value in place and returns an error).
func SavePiVersion(v string) error {
	if v == "" {
		return fmt.Errorf("pi version required")
	}
	path, err := PiVersionPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(v+"\n"), 0o600)
}

// rewriteWorkerDockerfileARG returns the staged Dockerfile with its
// PISO_PI_VERSION ARG pinned to v. Falls back to the input untouched when the
// ARG isn't found (caller decides whether that is an error).
func RewriteWorkerDockerfileARG(df, v string) string {
	lines := strings.Split(df, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "ARG PISO_PI_VERSION=") {
			lines[i] = "ARG PISO_PI_VERSION=" + v
		}
	}
	return strings.Join(lines, "\n")
}

// PiVersionFromDockerfile returns the ARG value pinned in a Dockerfile, or ""
// when absent.
func PiVersionFromDockerfile(df string) string {
	for _, line := range strings.Split(df, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "ARG PISO_PI_VERSION=") {
			return strings.TrimSpace(strings.TrimPrefix(t, "ARG PISO_PI_VERSION="))
		}
	}
	return ""
}