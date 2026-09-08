package workerhash

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContextHashStableAndChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entrypoint.sh"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "planning-watch.sh"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "context-watch.sh"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
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
}
