package pisoconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"piso/cli/internal/workerhash"
)

func writeTree(t *testing.T, root string) {
	t.Helper()
	compose := filepath.Join(root, "compose")
	if err := os.MkdirAll(compose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compose, "gateway.yaml"), []byte("name: piso\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestComposeHostPathForwardSlashes(t *testing.T) {
	if filepath.Separator != '\\' {
		if got := ComposeHostPath("/home/foo/.piso"); got != "/home/foo/.piso" {
			t.Fatalf("unix path: %q", got)
		}
		return
	}
	got := ComposeHostPath(`C:\Users\foo\.piso`)
	if strings.Contains(got, `\`) {
		t.Fatalf("backslash remains: %q", got)
	}
}

func TestHomePrefersPISO_HOME(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root)
	t.Setenv("PISO_HOME", root)
	got, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Home() = %q, want %q", got, want)
	}
}

func TestHomeRejectsEmptyPISO_HOME(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PISO_HOME", root)
	if _, err := Home(); err == nil {
		t.Fatal("expected error for PISO_HOME without compose/gateway.yaml")
	}
}

func TestHomeFromPrefixLayout(t *testing.T) {
	prefix := t.TempDir()
	share := filepath.Join(prefix, "share", "piso")
	writeTree(t, share)
	exe := filepath.Join(prefix, "bin", "piso")
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := homeFromPrefix(exe); got != share {
		t.Fatalf("homeFromPrefix = %q, want %q", got, share)
	}
}

func TestResolveHomePrefersCwdCheckoutOverPrefix(t *testing.T) {
	checkout := t.TempDir()
	writeTree(t, checkout)
	prefix := t.TempDir()
	share := filepath.Join(prefix, "share", "piso")
	writeTree(t, share)
	exe := filepath.Join(prefix, "bin", "piso")
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := resolveHome("", exe, checkout)
	if err != nil {
		t.Fatal(err)
	}
	if got != checkout {
		t.Fatalf("resolveHome = %q, want checkout %q", got, checkout)
	}
}

func TestResolveHomeUsesPrefixWhenCwdIsNotACheckout(t *testing.T) {
	prefix := t.TempDir()
	share := filepath.Join(prefix, "share", "piso")
	writeTree(t, share)
	exe := filepath.Join(prefix, "bin", "piso")
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := resolveHome("", exe, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != share {
		t.Fatalf("resolveHome = %q, want prefix share %q", got, share)
	}
}

func TestDataDirUsesPISO_DATA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	t.Setenv("PISO_DATA", dir)
	got, err := DataDir()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}
	st, err := os.Stat(got)
	if err != nil || !st.IsDir() {
		t.Fatalf("DataDir was not created: %v", err)
	}
}

func TestWriteWorkerComposeRendersPerWorkerEnv(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root)
	if err := os.MkdirAll(filepath.Join(root, "worker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "worker", "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "worker", "entrypoint.sh"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl := []byte("image: piso-worker\nPISO_WORKER_HASH: __WORKER_HASH__ CA_DIR/workers/PROJ-SLUG:/etc/piso:ro WORKER_BUILD_CONTEXT\n")
	if err := os.WriteFile(filepath.Join(root, "compose", "worker.yaml.tmpl"), tmpl, 0o600); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	t.Setenv("PISO_HOME", root)
	t.Setenv("PISO_DATA", data)
	build := filepath.Join(data, "worker-build")
	if err := os.MkdirAll(build, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range workerhash.Files {
		if err := os.WriteFile(filepath.Join(build, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tree := range workerhash.ExtraTrees {
		if err := os.MkdirAll(filepath.Join(build, tree), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(build, tree, "keep.go"), []byte("package x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	proj := Project{Dir: t.TempDir(), Slug: "demo"}
	legacyDir := filepath.Join(proj.Dir, ".piso")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyDir, "worker-demo.yaml")
	if err := os.WriteFile(legacy, []byte("leaked host paths\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := WriteWorkerCompose(proj)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(data, "compose", "worker-demo.yaml")
	if path != wantPath {
		t.Fatalf("compose path %s, want %s", path, wantPath)
	}
	if strings.HasPrefix(path, proj.Dir) {
		t.Fatalf("compose file must not live in the project share: %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "workers/demo:/etc/piso:ro") {
		t.Fatalf("compose missing per-worker env dir mount:\n%s", got)
	}
	if strings.Contains(got, "__WORKER_HASH__") {
		t.Fatalf("hash placeholder not replaced:\n%s", got)
	}
	if !strings.Contains(got, "PISO_WORKER_HASH:") {
		t.Fatalf("PISO_WORKER_HASH arg name was rewritten:\n%s", got)
	}
	if !strings.Contains(got, build) {
		t.Fatalf("compose must use staged worker-build, not repo worker/:\n%s", got)
	}
	if strings.Contains(got, filepath.Join(root, "worker")) {
		t.Fatalf("compose still pointed at repo worker/:\n%s", got)
	}
	if !strings.Contains(got, "image: piso-worker") {
		t.Fatalf("compose must pin the shared worker image:\n%s", got)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy in-share compose must be removed, stat: %v", err)
	}
}

// writeSkeletonTree builds a fake PISO_HOME with the worker skeleton and the
// relay source packages stageRelaySources copies.
func writeSkeletonTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root)
	for _, name := range workerhash.Files {
		if name == "package.json" {
			continue
		}
		if err := os.MkdirAll(filepath.Join(root, "worker"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "worker", name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{
		"internal/mcpchannel", "internal/technocore", "internal/ownfile", "cmd/piso-mcp",
	} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "keep.go"), []byte("package x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "keep_test.go"), []byte("package x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCopyWorkerSkeletonStagesRelay(t *testing.T) {
	root := writeSkeletonTree(t)
	// The repo's go.mod selects x/crypto v0.52.0; v0.41.0 survives only as a
	// /go.mod line (pruned content hash), exactly the state `go mod tidy`
	// leaves behind after a bump. The staged relay module must follow go.mod.
	gomod := "module piso\n\ngo 1.25.0\n\nrequire (\n" +
		"\tgithub.com/cloudflare/circl v1.6.1\n" +
		"\tgolang.org/x/crypto v0.52.0\n" +
		")\n\nrequire golang.org/x/sys v0.46.0 // indirect\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(gomod), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := "github.com/cloudflare/circl v1.6.1 h1:abc=\ngithub.com/cloudflare/circl v1.6.1/go.mod h1:def=\n" +
		"golang.org/x/crypto v0.41.0/go.mod h1:old=\n" +
		"golang.org/x/crypto v0.52.0 h1:abc=\ngolang.org/x/crypto v0.52.0/go.mod h1:def=\n" +
		"golang.org/x/sys v0.46.0 h1:abc=\ngolang.org/x/sys v0.46.0/go.mod h1:def=\n" +
		"modernc.org/sqlite v1.55.0 h1:nope=\n"
	if err := os.WriteFile(filepath.Join(root, "go.sum"), []byte(sum), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PISO_HOME", root)
	dest := t.TempDir()
	if err := CopyWorkerSkeleton(dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "relay-src", "cmd", "piso-mcp", "keep.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "relay-src", "cmd", "piso-mcp", "keep_test.go")); !os.IsNotExist(err) {
		t.Fatal("tests must not be staged")
	}
	mod, err := os.ReadFile(filepath.Join(dest, "relay-src", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mod), "sqlite") {
		t.Fatalf("relay module pulled sqlite:\n%s", mod)
	}
	if !strings.Contains(string(mod), "golang.org/x/crypto v0.52.0") || strings.Contains(string(mod), "v0.41.0") {
		t.Fatalf("relay module must track the repo go.mod:\n%s", mod)
	}
	gotSum, err := os.ReadFile(filepath.Join(dest, "relay-src", "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gotSum), "sqlite") || !strings.Contains(string(gotSum), "circl v1.6.1 ") || !strings.Contains(string(gotSum), "circl v1.6.1/go.mod") {
		t.Fatalf("sum filter:\n%s", gotSum)
	}
	if !strings.Contains(string(gotSum), "golang.org/x/crypto v0.52.0 h1:") {
		t.Fatalf("staged sum lost the selected module hash:\n%s", gotSum)
	}
}

func TestCopyWorkerSkeletonRefusesRelaySumWithoutContentHash(t *testing.T) {
	root := writeSkeletonTree(t)
	gomod := "module piso\n\ngo 1.25.0\n\nrequire (\n" +
		"\tgithub.com/cloudflare/circl v1.6.1\n" +
		"\tgolang.org/x/crypto v0.52.0\n" +
		")\n\nrequire golang.org/x/sys v0.46.0 // indirect\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(gomod), 0o600); err != nil {
		t.Fatal(err)
	}
	// Every module hash except the selected x/crypto content hash: the build
	// would fail on "missing go.sum entry", so staging must say so first.
	sum := "github.com/cloudflare/circl v1.6.1 h1:abc=\ngithub.com/cloudflare/circl v1.6.1/go.mod h1:def=\n" +
		"golang.org/x/crypto v0.52.0/go.mod h1:def=\n" +
		"golang.org/x/sys v0.46.0 h1:abc=\ngolang.org/x/sys v0.46.0/go.mod h1:def=\n"
	if err := os.WriteFile(filepath.Join(root, "go.sum"), []byte(sum), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PISO_HOME", root)
	err := CopyWorkerSkeleton(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "golang.org/x/crypto v0.52.0") {
		t.Fatalf("want a staging error naming the missing hash, got %v", err)
	}
}

func TestEnsureWorkerPlaceholdersEnvCreatesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PISO_DATA", dir)
	if err := EnsureWorkerPlaceholdersEnv("myproj"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "workers", "myproj", PlaceholdersEnvName))
	if err != nil || st.IsDir() {
		t.Fatalf("expected file: %v", err)
	}
}

func TestEnsurePiProfileFilesReplacesDockerDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PISO_DATA", dir)
	prof := filepath.Join(dir, "pi-profile")
	if err := os.MkdirAll(filepath.Join(prof, "settings.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePiProfileFiles(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(prof, "settings.json"))
	if err != nil || st.IsDir() {
		t.Fatalf("settings.json must be a file: %v", err)
	}
}

func TestEnsureCAFilesReplacesDockerDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PISO_DATA", dir)
	crt := filepath.Join(dir, "ca.crt")
	if err := os.Mkdir(crt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCAFiles(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(crt)
	if err != nil || st.IsDir() {
		t.Fatalf("ca.crt must be a file, stat=%v err=%v", st, err)
	}
	key := filepath.Join(dir, "ca.key")
	st, err = os.Stat(key)
	if err != nil || st.IsDir() {
		t.Fatalf("ca.key must be a file, stat=%v err=%v", st, err)
	}
}

func TestEnsurePlaceholdersEnvCreatesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PISO_DATA", dir)
	if err := EnsurePlaceholdersEnv(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, PlaceholdersEnvName))
	if err != nil || st.IsDir() {
		t.Fatalf("expected file: %v", err)
	}
	if err := EnsurePlaceholdersEnv(); err != nil {
		t.Fatal(err)
	}
}

func TestFindTreeRootWalksParents(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root)
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := findTreeRoot(nested); got != root {
		t.Fatalf("findTreeRoot = %q, want %q", got, root)
	}
}
