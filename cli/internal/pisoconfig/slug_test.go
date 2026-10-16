package pisoconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSanitizeSlug(t *testing.T) {
	cases := map[string]string{
		"my-app":     "my-app",
		"My App":     "My-App",
		"...dots...": "dots",
		"":           "",
		"a_b.c":      "a_b.c",
		"foo/bar":    "foo-bar",
	}
	for in, want := range cases {
		if got := SanitizeSlug(in); got != want {
			t.Fatalf("SanitizeSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeWorkerName(t *testing.T) {
	if got := NormalizeWorkerName("piso-worker-demo"); got != "demo" {
		t.Fatalf("strip prefix: %q", got)
	}
	if got := NormalizeWorkerName("PISO-WORKER-Demo"); got != "Demo" {
		t.Fatalf("case-insensitive prefix: %q", got)
	}
	if got := NormalizeWorkerName("  my repo  "); got != "my-repo" {
		t.Fatalf("spaces: %q", got)
	}
}

func TestSlugFromDir(t *testing.T) {
	if got := SlugFromDir("/home/you/my app"); got != "my-app" {
		t.Fatalf("SlugFromDir = %q", got)
	}
	if got := SlugFromDir("/"); got != "workspace" {
		t.Fatalf("root → workspace, got %q", got)
	}
}

func TestSlugFromGitRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:org/piso.git":               "piso",
		"git@github-complex-gh:complex-gh/piso.git": "piso",
		"https://github.com/org/my-repo.git":        "my-repo",
		"https://github.com/org/my-repo.git/":       "my-repo",
		"ssh://git@github.com/org/repo.git":         "repo",
		"https://github.com/org/repo":               "repo",
		"/abs/path/to/piso.git":                     "piso",
		"":                                          "",
	}
	for in, want := range cases {
		if got := slugFromGitRemote(in); got != want {
			t.Fatalf("slugFromGitRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGitRepoSlug(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if GitRepoSlug(dir) != "" {
		t.Fatal("no .git → empty")
	}
	runGit(t, dir, "init", "-q")
	if GitRepoSlug(dir) != "" {
		t.Fatal("no origin → empty")
	}
	runGit(t, dir, "remote", "add", "origin", "git@github.com:org/cool-repo.git")
	if got := GitRepoSlug(dir); got != "cool-repo" {
		t.Fatalf("GitRepoSlug = %q, want cool-repo", got)
	}
	if got := DefaultSlug(dir); got != "cool-repo" {
		t.Fatalf("DefaultSlug prefers git: %q", got)
	}
}

func TestDefaultSlugFallsBackToDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "folder-name")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := DefaultSlug(dir); got != "folder-name" {
		t.Fatalf("DefaultSlug = %q", got)
	}
}

func TestDecideWorkerSlugLocked(t *testing.T) {
	_, err := DecideWorkerSlug(NameDecision{Saved: "keep", NameFlag: "other"})
	if err == nil {
		t.Fatal("expected rename error")
	}
	got, err := DecideWorkerSlug(NameDecision{Saved: "keep", NameFlag: "keep"})
	if err != nil || got != "keep" {
		t.Fatalf("matching --name: %q %v", got, err)
	}
	got, err = DecideWorkerSlug(NameDecision{Existing: "old", Proposed: "gitname"})
	if err != nil || got != "old" {
		t.Fatalf("existing wins: %q %v", got, err)
	}
}

func TestDecideWorkerSlugFirstUp(t *testing.T) {
	got, err := DecideWorkerSlug(NameDecision{Proposed: "from-git", NameFlag: "custom"})
	if err != nil || got != "custom" {
		t.Fatalf("--name: %q %v", got, err)
	}
	got, err = DecideWorkerSlug(NameDecision{Proposed: "from-git", Interactive: true, Typed: ""})
	if err != nil || got != "from-git" {
		t.Fatalf("enter accepts: %q %v", got, err)
	}
	got, err = DecideWorkerSlug(NameDecision{Proposed: "from-git", Interactive: true, Typed: "typed-name"})
	if err != nil || got != "typed-name" {
		t.Fatalf("typed override: %q %v", got, err)
	}
	got, err = DecideWorkerSlug(NameDecision{Proposed: "from-git"})
	if err != nil || got != "from-git" {
		t.Fatalf("non-interactive: %q %v", got, err)
	}
	if _, err := DecideWorkerSlug(NameDecision{Proposed: "monitor"}); err == nil {
		t.Fatal("reserved")
	}
	if _, err := DecideWorkerSlug(NameDecision{NameFlag: "piso-worker-monitor"}); err == nil {
		t.Fatal("reserved via prefix")
	}
}

func TestProjectSlugRoundTrip(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	dir := t.TempDir()
	if _, ok := LoadProjectSlug(dir); ok {
		t.Fatal("empty mapping")
	}
	if err := SaveProjectSlug(dir, "demo"); err != nil {
		t.Fatal(err)
	}
	got, ok := LoadProjectSlug(dir)
	if !ok || got != "demo" {
		t.Fatalf("LoadProjectSlug = %q %v", got, ok)
	}
	owner, ok := DirForSlug("demo")
	if !ok || owner != canonDir(dir) {
		t.Fatalf("DirForSlug = %q %v", owner, ok)
	}
}

func TestComposeSlugForDir(t *testing.T) {
	data := t.TempDir()
	t.Setenv("PISO_DATA", data)
	dir := t.TempDir()
	compose := filepath.Join(data, "compose")
	if err := os.MkdirAll(compose, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "volumes:\n      - \"" + ComposeHostPath(dir) + ":/workspace\"\n"
	if err := os.WriteFile(filepath.Join(compose, "worker-fromgit.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := ComposeSlugForDir(dir)
	if !ok || got != "fromgit" {
		t.Fatalf("ComposeSlugForDir = %q %v", got, ok)
	}
	if !SlugTakenByOther("fromgit", t.TempDir()) {
		t.Fatal("slug should be taken by the compose file's dir")
	}
	if SlugTakenByOther("fromgit", dir) {
		t.Fatal("same dir is not taken")
	}
}

func TestResolveUsesSavedSlug(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	dir := t.TempDir()
	if err := SaveProjectSlug(dir, "locked"); err != nil {
		t.Fatal(err)
	}
	proj, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if proj.Slug != "locked" {
		t.Fatalf("Resolve slug = %q, want locked", proj.Slug)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitCleanEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
