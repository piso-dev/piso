// Package pisoconfig resolves piso project/dir/gateway paths and the compose
// files the CLI drives. Kept free of Docker logic so the CLI stays testable.
package pisoconfig

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"piso/cli/internal/workerhash"
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
// Resolution order: PISO_HOME, a checkout walked from the cwd (so `piso up`
// inside this repo uses the local Dockerfile, not a stale make-install copy),
// then PREFIX/share/piso next to the binary, then a checkout walked from the
// executable.
func Home() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	return resolveHome(os.Getenv("PISO_HOME"), exe, cwd)
}

// resolveHome is Home() with injectable paths so tests can cover prefix vs cwd.
func resolveHome(pisoHome, exe, cwd string) (string, error) {
	if v := strings.TrimSpace(pisoHome); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", fmt.Errorf("PISO_HOME: %w", err)
		}
		if !isTreeRoot(abs) {
			return "", fmt.Errorf("PISO_HOME %s is missing compose/gateway.yaml", abs)
		}
		return abs, nil
	}
	if cwd != "" {
		if h := findTreeRoot(cwd); h != "" {
			return h, nil
		}
	}
	if exe != "" {
		if h := homeFromPrefix(exe); h != "" {
			return h, nil
		}
		if h := findTreeRoot(filepath.Dir(exe)); h != "" {
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
	// Staged under PISO_DATA so host extension package.json is not written
	// into the repo / make-install share tree.
	workerDir, err := WorkerBuildDir()
	if err != nil {
		return "", err
	}
	hash, err := workerhash.ContextHash(workerDir)
	if err != nil {
		return "", fmt.Errorf("worker context hash: %w (run piso up so the build context is staged)", err)
	}
	out = strings.ReplaceAll(out, "WORKER_BUILD_CONTEXT", workerDir)
	// __WORKER_HASH__ must not be a substring of PISO_WORKER_HASH (ReplaceAll
	// of WORKER_HASH previously rewrote the ARG name and the label stayed "dev").
	out = strings.ReplaceAll(out, "__WORKER_HASH__", hash)
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

// WorkerBuildDir is ~/.piso/worker-build: Dockerfile + entrypoint + the host
// extension package.json staged for `docker build`.
func WorkerBuildDir() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worker-build"), nil
}

// CopyWorkerSkeleton copies image files from the piso home worker tree into
// dest (the staged build context). package.json is written separately.
func CopyWorkerSkeleton(dest string) error {
	home, err := Home()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	src := filepath.Join(home, "worker")
	for _, name := range []string{"Dockerfile", "entrypoint.sh", "planning-watch.sh"} {
		if err := copyFile(filepath.Join(src, name), filepath.Join(dest, name)); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
	}
	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
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

// PlaceholdersEnvName is the worker-safe file (KEY=piso_… only) in the data dir.
const PlaceholdersEnvName = "placeholders.env"

// PlaceholdersEnvPath is ~/.piso/placeholders.env (legacy global file).
func PlaceholdersEnvPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, PlaceholdersEnvName), nil
}

// WorkerPlaceholdersEnvPath is ~/.piso/workers/<slug>/placeholders.env.
func WorkerPlaceholdersEnvPath(slug string) (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "workers", slug, PlaceholdersEnvName), nil
}

// EnsureWorkerPlaceholdersEnv creates workers/<slug>/placeholders.env so Docker
// bind-mounts a file, not a directory.
func EnsureWorkerPlaceholdersEnv(slug string) error {
	path, err := WorkerPlaceholdersEnvPath(slug)
	if err != nil {
		return err
	}
	return ensurePlaceholderFile(path)
}

// EnsurePiProfileFiles creates empty profile files so Docker bind-mounts files.
func EnsurePiProfileFiles() error {
	dir, err := DataDir()
	if err != nil {
		return err
	}
	prof := filepath.Join(dir, "pi-profile")
	if err := os.MkdirAll(prof, 0o700); err != nil {
		return err
	}
	if err := writeFileIfMissing(filepath.Join(prof, "settings.json"), []byte("{}\n")); err != nil {
		return err
	}
	return writeFileIfMissing(filepath.Join(prof, "models.json"), []byte("{\n  \"providers\": {}\n}\n"))
}

func writeFileIfMissing(path string, body []byte) error {
	st, err := os.Stat(path)
	if err == nil {
		if st.IsDir() {
			return fmt.Errorf("%s is a directory; remove it so piso can mount a file", path)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// EnsurePlaceholdersEnv creates the legacy global file if missing.
func EnsurePlaceholdersEnv() error {
	path, err := PlaceholdersEnvPath()
	if err != nil {
		return err
	}
	return ensurePlaceholderFile(path)
}

func ensurePlaceholderFile(path string) error {
	st, err := os.Stat(path)
	if err == nil {
		if st.IsDir() {
			return fmt.Errorf("%s is a directory; remove it so piso can mount a file", path)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("# piso placeholders only — never real secrets.\n"), 0o600)
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

// RouteRec mirrors the gateway's stored ingress route (GET /api/v1/routes), so
// the CLI can list the `name.piso.local → worker:port` routes targeting each
// worker. Fields match store.RouteRec in the gateway (id/name/worker/port).
type RouteRec struct {
	ID	   string `json:"id"`
	Name	 string `json:"name"`
	Worker	 string `json:"worker"`
	Port	   int	  `json:"port"`
	Note	   string `json:"note,omitempty"`
}

// WorkerIn is the host-CLI registration payload for the slug↔IP registry.
type WorkerIn struct {
	Name string   `json:"name"`
	Slug string   `json:"slug"`
	IPs  []string `json:"ips"`
}

type LogRecord struct {
	ID       string    `json:"id"`
	Worker   string    `json:"worker"`
	Slug     string    `json:"slug,omitempty"`
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
