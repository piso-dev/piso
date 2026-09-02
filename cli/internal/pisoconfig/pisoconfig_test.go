package pisoconfig

import (
	"os"
	"path/filepath"
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
