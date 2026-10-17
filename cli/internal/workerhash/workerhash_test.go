package workerhash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextHashStableAndChanges(t *testing.T) {
	dir := t.TempDir()
	for _, name := range Files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tree := range ExtraTrees {
		if err := os.MkdirAll(filepath.Join(dir, tree, "cmd"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, tree, "cmd", "main.go"), []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := ContextHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ContextHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a == "" {
		t.Fatalf("unstable hash %q %q", a, b)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := ContextHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c == a {
		t.Fatal("hash should change when Dockerfile changes")
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{\"a\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := ContextHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d == c {
		t.Fatal("hash should change when package.json changes")
	}
	if err := os.WriteFile(filepath.Join(dir, "relay-src", "cmd", "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := ContextHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e == d {
		t.Fatal("hash should change when relay source changes")
	}
	if err := os.WriteFile(filepath.Join(dir, "project-footer.ts"), []byte("// updated footer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := ContextHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f == e {
		t.Fatal("hash should change when project-footer.ts changes")
	}
}

// TestFilesCoverDockerfileCopySources guards the staging list against drift:
// CopyWorkerSkeleton only stages names in Files, so a Dockerfile COPY whose
// source is missing here fails the image build with
// "failed to calculate checksum ... not found" (supervisor-session.py was
// exactly that bug).
func TestFilesCoverDockerfileCopySources(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "worker", "Dockerfile"))
	if err != nil {
		t.Skipf("repo worker/Dockerfile not available: %v", err)
	}
	staged := make(map[string]bool, len(Files))
	for _, name := range Files {
		staged[name] = true
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "COPY") {
			continue
		}
		if strings.HasPrefix(fields[1], "--from") {
			continue // multi-stage build output, not a context file
		}
		src := fields[1]
		if src == "relay-src" || strings.HasPrefix(src, "relay-src/") {
			continue // staged as trees via ExtraTrees
		}
		if !staged[src] {
			t.Errorf("Dockerfile COPYs %q but it is not in workerhash.Files (the staged build context would omit it)", src)
		}
	}
}
