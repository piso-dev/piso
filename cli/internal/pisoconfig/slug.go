package pisoconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	// ReservedSlugMonitor is the gateway-sidecar worker; project slugs must not collide.
	ReservedSlugMonitor = "monitor"
	projectsFileName    = "projects.json"
)

var slugUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

// SanitizeSlug collapses a candidate into a docker/compose-safe identifier.
// Empty after cleaning is returned as-is so callers can reject it.
func SanitizeSlug(s string) string {
	s = slugUnsafe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-.")
}

// NormalizeWorkerName accepts a slug or a full `piso-worker-<slug>` name.
func NormalizeWorkerName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= len("piso-worker-") && strings.EqualFold(s[:len("piso-worker-")], "piso-worker-") {
		s = s[len("piso-worker-"):]
	}
	return SanitizeSlug(s)
}

// SlugFromDir is the directory-basename identifier used before git/--name.
func SlugFromDir(dir string) string {
	base := SanitizeSlug(filepath.Base(dir))
	if base == "" {
		return "workspace"
	}
	return base
}

// ReservedSlug reports names that would collide with built-in containers.
func ReservedSlug(slug string) bool {
	return strings.EqualFold(strings.TrimSpace(slug), ReservedSlugMonitor)
}

// GitRepoSlug returns a slug from origin's repo name when dir is a git root
// (contains `.git`). Empty means "not a repo" or "could not parse a name".
func GitRepoSlug(dir string) string {
	st, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return ""
	}
	if !st.IsDir() && !st.Mode().IsRegular() {
		return ""
	}
	url := gitOutput(dir, "remote", "get-url", "origin")
	if slug := slugFromGitRemote(url); slug != "" && !ReservedSlug(slug) {
		return slug
	}
	return ""
}

// DefaultSlug prefers the git origin repo name, then the directory basename.
func DefaultSlug(dir string) string {
	if g := GitRepoSlug(dir); g != "" {
		return g
	}
	return SlugFromDir(dir)
}

func slugFromGitRemote(raw string) string {
	url := strings.TrimSpace(raw)
	url = strings.TrimSuffix(url, "/")
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimSuffix(url, "/")
	if url == "" {
		return ""
	}
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
		if at := strings.LastIndex(url, "@"); at >= 0 {
			url = url[at+1:]
		}
	} else if i := strings.Index(url, ":"); i >= 0 && !strings.Contains(url[:i], "/") {
		url = url[i+1:]
	}
	url = strings.ReplaceAll(url, "\\", "/")
	url = strings.Trim(url, "/")
	if url == "" {
		return ""
	}
	base := url
	if i := strings.LastIndex(url, "/"); i >= 0 {
		base = url[i+1:]
	}
	return SanitizeSlug(base)
}

func gitOutput(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitCleanEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitCleanEnv() []string {
	out := make([]string, 0, 8)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GIT_DIR=") || strings.HasPrefix(e, "GIT_WORK_TREE=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// NameDecision is the inputs to pick a worker slug for `piso up`.
type NameDecision struct {
	Dir         string
	NameFlag    string
	Saved       string // mapping for this dir
	Existing    string // already provisioned slug for this dir (compose/container)
	Proposed    string // DefaultSlug
	Interactive bool
	Typed       string // user input when Interactive; empty means accept Proposed
}

// DecideWorkerSlug picks the slug. A saved or existing worker is locked:
// --name and typed input cannot rename it. NameFlag wins over the prompt
// on a first up.
func DecideWorkerSlug(d NameDecision) (string, error) {
	if d.Saved != "" {
		if err := rejectRename(d.NameFlag, d.Saved); err != nil {
			return "", err
		}
		return d.Saved, nil
	}
	if d.Existing != "" {
		if err := rejectRename(d.NameFlag, d.Existing); err != nil {
			return "", err
		}
		return d.Existing, nil
	}
	chosen := d.Proposed
	if flag := NormalizeWorkerName(d.NameFlag); flag != "" {
		chosen = flag
	} else if d.Interactive {
		if typed := NormalizeWorkerName(d.Typed); typed != "" {
			chosen = typed
		}
	}
	return validateNewSlug(chosen)
}

func rejectRename(nameFlag, locked string) error {
	flag := NormalizeWorkerName(nameFlag)
	if flag != "" && flag != locked {
		return fmt.Errorf("worker already named %q (--name is only for the first piso up)", locked)
	}
	return nil
}

func validateNewSlug(slug string) (string, error) {
	slug = NormalizeWorkerName(slug)
	if slug == "" {
		return "", fmt.Errorf("worker name is empty")
	}
	if ReservedSlug(slug) {
		return "", fmt.Errorf("worker name %q is reserved", slug)
	}
	return slug, nil
}

func projectsPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, projectsFileName), nil
}

func loadProjects() (map[string]string, error) {
	path, err := projectsPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("projects.json: %w", err)
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

func saveProjects(m map[string]string) error {
	path, err := projectsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func canonDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	return filepath.Clean(abs)
}

func sameDir(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// LoadProjectSlug returns the locked slug for dir, if any.
func LoadProjectSlug(dir string) (string, bool) {
	m, err := loadProjects()
	if err != nil {
		return "", false
	}
	slug, ok := m[canonDir(dir)]
	slug = strings.TrimSpace(slug)
	if !ok || slug == "" {
		return "", false
	}
	return slug, true
}

// SaveProjectSlug locks dir to slug for later attach/down/up.
func SaveProjectSlug(dir, slug string) error {
	dir = canonDir(dir)
	slug = NormalizeWorkerName(slug)
	if slug == "" {
		return fmt.Errorf("worker name is empty")
	}
	m, err := loadProjects()
	if err != nil {
		return err
	}
	if cur, ok := m[dir]; ok && cur == slug {
		return nil
	}
	m[dir] = slug
	return saveProjects(m)
}

// DirForSlug returns another project path already locked to slug.
func DirForSlug(slug string) (string, bool) {
	slug = NormalizeWorkerName(slug)
	m, err := loadProjects()
	if err != nil {
		return "", false
	}
	for dir, s := range m {
		if s == slug {
			return dir, true
		}
	}
	return "", false
}

// ComposeSlugForDir finds a generated worker compose file whose /workspace
// bind mount is dir. Used to treat a previously `piso up`'d project as existing
// even after `piso down` (container gone, compose file remains).
func ComposeSlugForDir(dir string) (string, bool) {
	dir = canonDir(dir)
	needle := ComposeHostPath(dir) + ":/workspace"
	data, err := DataDir()
	if err != nil {
		return "", false
	}
	matches, err := filepath.Glob(filepath.Join(data, "compose", "worker-*.yaml"))
	if err != nil {
		return "", false
	}
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if !strings.Contains(string(raw), needle) {
			continue
		}
		base := filepath.Base(path)
		slug := strings.TrimSuffix(strings.TrimPrefix(base, "worker-"), ".yaml")
		if slug != "" {
			return slug, true
		}
	}
	return "", false
}

// ComposeDirForSlug reads the /workspace source path out of worker-<slug>.yaml.
func ComposeDirForSlug(slug string) (string, bool) {
	path, err := WorkerComposePath(slug)
	if err != nil {
		return "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	const sep = ":/workspace"
	s := string(raw)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.Trim(line, `"`)
		if i := strings.Index(line, sep); i > 0 {
			return line[:i], true
		}
	}
	return "", false
}

// SlugTakenByOther reports whether slug is already bound to a different dir.
func SlugTakenByOther(slug, dir string) bool {
	dir = canonDir(dir)
	slug = NormalizeWorkerName(slug)
	if other, ok := DirForSlug(slug); ok && !sameDir(other, dir) {
		return true
	}
	if other, ok := ComposeDirForSlug(slug); ok && !sameDir(other, dir) {
		return true
	}
	return false
}
