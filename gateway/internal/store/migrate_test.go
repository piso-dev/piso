package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyMigrationsV0ToV1(t *testing.T) {
	st := State{}
	if !applyMigrations(&st) {
		t.Fatal("expected rewrite")
	}
	if st.Version != 1 {
		t.Fatalf("version %d", st.Version)
	}
	if st.Secrets == nil || st.Routes == nil {
		t.Fatal("slices should be non-nil")
	}
	if applyMigrations(&st) {
		t.Fatal("v1 should be a no-op")
	}
}

func TestLoadMigratesLegacyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	legacy := `{"secrets":[{"id":"s1","name":"x","placeholder":"piso_x_abcdef","value":"secret"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := New(path, filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if st.state.Version != CurrentStateVersion {
		t.Fatalf("version %d", st.state.Version)
	}
	if len(st.Secrets()) != 1 || st.Secrets()[0].ID != "s1" {
		t.Fatalf("lost secret: %+v", st.Secrets())
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), `"version": 1`) {
		t.Fatalf("migrated file missing version: %s", onDisk)
	}
}

func TestNewEmptyStateWritesCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := New(path, filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if st.state.Version != CurrentStateVersion {
		t.Fatalf("version %d", st.state.Version)
	}
}
