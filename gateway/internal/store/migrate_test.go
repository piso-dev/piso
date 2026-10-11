package store

import (
	"fmt"
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
	if st.Version != CurrentStateVersion {
		t.Fatalf("version %d", st.Version)
	}
	if st.Secrets == nil || st.Routes == nil {
		t.Fatal("slices should be non-nil")
	}
	if applyMigrations(&st) {
		t.Fatal("already-current should be a no-op")
	}
}

func TestLoadMigratesLegacyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	legacy := `{"secrets":[{"id":"s1","name":"x","placeholder":"piso_x_abcdef","value":"secret"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := New(path, filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 10)
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
	if !strings.Contains(string(onDisk), fmt.Sprintf("\"version\": %d", CurrentStateVersion)) {
		t.Fatalf("migrated file missing current version: %s", onDisk)
	}
}

func TestNewEmptyStateWritesCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := New(path, filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if st.state.Version != CurrentStateVersion {
		t.Fatalf("version %d", st.state.Version)
	}
}

func TestApplyMigrationsV3ToV4DefaultsOriginToExpose(t *testing.T) {
	st := State{Version: 3, Routes: []RouteRec{
		RouteRec{ID: "r1", Name: "plan-demo", Worker: "piso-worker-demo", Port: 19432},
	}}
	if !applyMigrations(&st) {
		t.Fatal("expected rewrite")
	}
	if st.Version != CurrentStateVersion {
		t.Fatalf("version %d", st.Version)
	}
	if st.Routes[0].Origin != AutoRouteOriginExpose {
		t.Fatalf("legacy route origin %q", st.Routes[0].Origin)
	}
}

func TestApplyMigrationsV8ToV9PromotesProviderToStack(t *testing.T) {
	legacy := &PiProviderRec{SecretID: "sec-1", BaseURL: "https://api.example/v1", DefaultModel: "m1"}
	st := State{Version: 8, PiProvider: legacy}
	if !applyMigrations(&st) {
		t.Fatal("expected rewrite")
	}
	if st.Version != CurrentStateVersion {
		t.Fatalf("version %d", st.Version)
	}
	if st.PiProvider != nil {
		t.Fatalf("legacy pointer not cleared: %+v", st.PiProvider)
	}
	if len(st.PiProviders) != 1 || st.ActivePiProviderID != "piso" {
		t.Fatalf("stack: %+v active %q", st.PiProviders, st.ActivePiProviderID)
	}
	if st.PiProviders[0].DefaultModel != "m1" || st.PiProviders[0].BaseURL != "https://api.example/v1" {
		t.Fatalf("promoted record: %+v", st.PiProviders[0])
	}
	// The migration is idempotent.
	if applyMigrations(&st) {
		t.Fatal("already-current should be a no-op")
	}
}
