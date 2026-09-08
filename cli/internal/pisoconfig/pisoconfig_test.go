package pisoconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	tmpl := []byte("image: piso-worker\nPISO_WORKER_HASH: __WORKER_HASH__ CA_DIR/workers/PROJ-SLUG/placeholders.env WORKER_BUILD_CONTEXT\n")
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
	for _, name := range []string{"Dockerfile", "entrypoint.sh", "planning-watch.sh", "context-watch.sh", "ports-watch.sh", "package.json"} {
		if err := os.WriteFile(filepath.Join(build, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	proj := Project{Dir: t.TempDir(), Slug: "demo"}
	path, err := WriteWorkerCompose(proj)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "workers/demo/placeholders.env") {
		t.Fatalf("compose missing per-worker env mount:\n%s", got)
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
