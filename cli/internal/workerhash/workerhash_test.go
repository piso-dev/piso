package workerhash

import (
	"os"
	"path/filepath"
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
