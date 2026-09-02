// Package pisoconfig resolves piso project/dir/gateway paths and the compose
// files the CLI drives. Kept free of Docker logic so the CLI stays testable.
package pisoconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Project is a resolved piso workspace: the host dir mounted into the worker.
type Project struct {
	Dir  string // absolute host dir mounted at /workspace
	Slug string // safe container/compose name derived from dir
}

// Resolve builds a Project from an absolute directory.
func Resolve(dir string) (Project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Project{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return Project{}, err
	}
	if !st.IsDir() {
		return Project{}, fmt.Errorf("%s is not a directory", abs)
	}
	return Project{Dir: abs, Slug: slug(abs)}, nil
}

// ProjectFromCWD resolves the project from the current working directory.
func ProjectFromCWD() (Project, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return Project{}, err
	}
	return Resolve(cwd)
}

// slug converts a workspace path into a safe identifier.
func slug(dir string) string {
	base := filepath.Base(dir)
	// strip unsafe characters
	re := regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)
	base = re.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-.")
	if base == "" || base == "." {
		base = "workspace"
	}
	return base
}

// WorkerName is the deterministic container name (must match the compose
// template's container_name).
func (p Project) WorkerName() string { return "piso-worker-" + p.Slug }

// Home is the piso share/repo root: compose/, worker/, gateway/.
// Resolution order: PISO_HOME, PREFIX/share/piso next to the binary
// (make install), then a checkout containing compose/gateway.yaml
// walked from the executable or the cwd.
func Home() (string, error) {
	if v := strings.TrimSpace(os.Getenv("PISO_HOME")); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", fmt.Errorf("PISO_HOME: %w", err)
		}
		if !isTreeRoot(abs) {
			return "", fmt.Errorf("PISO_HOME %s is missing compose/gateway.yaml", abs)
		}
		return abs, nil
	}
	if exe, err := os.Executable(); err == nil {
		if h := homeFromPrefix(exe); h != "" {
			return h, nil
		}
		if h := findTreeRoot(filepath.Dir(exe)); h != "" {
			return h, nil
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		if h := findTreeRoot(cwd); h != "" {
			return h, nil
		}
	}
	return "", fmt.Errorf("cannot find piso home (compose/gateway.yaml); run `make install` or set PISO_HOME")
}

// DataDir is the host directory mounted into the gateway at /data (state, CA,
// request log). PISO_DATA overrides; otherwise ~/.piso. Created at 0700.
func DataDir() (string, error) {
	if v := strings.TrimSpace(os.Getenv("PISO_DATA")); v != "" {
		return ensureDir(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}
	return ensureDir(filepath.Join(home, ".piso"))
}

// GatewayCompose returns the path to the shared gateway compose file.
func GatewayCompose() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	p := filepath.Join(home, "compose", "gateway.yaml")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("gateway compose: %w", err)
	}
	return p, nil
}

// WriteWorkerCompose renders the worker compose template with this project's
// values into <project>/.piso/worker-<slug>.yaml.
func WriteWorkerCompose(p Project) (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	dataDir, err := DataDir()
	if err != nil {
		return "", err
	}
	tmpl := filepath.Join(home, "compose", "worker.yaml.tmpl")
	raw, err := os.ReadFile(tmpl)
	if err != nil {
		return "", fmt.Errorf("worker compose template: %w (run `make install`?)", err)
	}
	out := string(raw)
	out = strings.ReplaceAll(out, "PROJ-SLUG", p.Slug)
	out = strings.ReplaceAll(out, "PROJECT_DIR", p.Dir)
	out = strings.ReplaceAll(out, "WORKER_BUILD_CONTEXT", filepath.Join(home, "worker"))
	// CA is written by the gateway into the machine-level data dir, not the project.
	out = strings.ReplaceAll(out, "CA_DIR", dataDir)
	out = strings.ReplaceAll(out, "GATEWAY_CONTROL", GatewayURL())

	pisoDir := filepath.Join(p.Dir, ".piso")
	if err := os.MkdirAll(pisoDir, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(pisoDir, "worker-"+p.Slug+".yaml")
	if err := os.WriteFile(dest, []byte(out), 0o600); err != nil {
		return "", err
	}
	return dest, nil
}

// homeFromPrefix implements the make-install layout:
//
//	$(PREFIX)/bin/piso  →  $(PREFIX)/share/piso
func homeFromPrefix(exe string) string {
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	share := filepath.Clean(filepath.Join(filepath.Dir(resolved), "..", "share", "piso"))
	if isTreeRoot(share) {
		return share
	}
	return ""
}

// findTreeRoot walks parents looking for compose/gateway.yaml (a repo checkout).
func findTreeRoot(start string) string {
	dir := start
	for i := 0; i < 8; i++ {
		if isTreeRoot(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// isTreeRoot reports whether dir has the compose file `make install` copies.
func isTreeRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "compose", "gateway.yaml"))
	return err == nil
}

// ensureDir creates dir at 0700 and returns its absolute path.
func ensureDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	return abs, nil
}

// GatewayURL is the control-plane base URL (override with PISO_GATEWAY).
func GatewayURL() string {
	if v := os.Getenv("PISO_GATEWAY"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:8081"
}

// API shapes mirror the gateway's model (safe on the client side; the
// gateway never sends real secret values over GET).
type SecretSummary struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Placeholder  string   `json:"placeholder"`
	AllowedHosts []string `json:"allowedHosts"`
}

type SecretIn struct {
	Name         string   `json:"name"`
	Placeholder  string   `json:"placeholder"`
	Value        string   `json:"value"`
	AllowedHosts []string `json:"allowedHosts"`
}

type RouteIn struct {
	Name   string `json:"name"`
	Worker string `json:"worker"`
	Port   int    `json:"port"`
}

type LogRecord struct {
	ID       string    `json:"id"`
	Worker   string    `json:"worker"`
	Ts       time.Time `json:"ts"`
	Method   string    `json:"method"`
	Scheme   string    `json:"scheme"`
	Host     string    `json:"host"`
	Path     string    `json:"path"`
	Status   int       `json:"status"`
	Action   string    `json:"action"`
	Reasons  []string  `json:"reasons"`
	Findings []Finding `json:"findings"`
}

type Finding struct {
	Kind string `json:"kind"`
}
