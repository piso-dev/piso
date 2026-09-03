package pisoconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImportLegacyDataCopiesMissingOnly(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "state.json"), []byte(`{"secrets":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "patterns.json"), []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "state.json"), []byte(`{"keep":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	copied, err := ImportLegacyData(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 1 || copied[0] != "patterns.json" {
		t.Fatalf("copied %v, want [patterns.json]", copied)
	}
	keep, err := os.ReadFile(filepath.Join(dst, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(keep) != `{"keep":true}` {
		t.Fatalf("overwrote live state: %s", keep)
	}
	got, err := os.ReadFile(filepath.Join(dst, "patterns.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `[]` {
		t.Fatalf("patterns: %s", got)
	}
}

func TestImportLegacyDataSameDirIsNoop(t *testing.T) {
	dir := t.TempDir()
	copied, err := ImportLegacyData(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 0 {
		t.Fatalf("copied %v", copied)
	}
}

func TestImportLegacyDataMissingSource(t *testing.T) {
	if _, err := ImportLegacyData(filepath.Join(t.TempDir(), "nope"), t.TempDir()); err == nil {
		t.Fatal("expected error")
	}
}
