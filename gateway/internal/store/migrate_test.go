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

// A secret whose empty host list used to mean "any host" must keep working
// after the mandatory-binding change: the migration materializes the "*" that
// was previously implicit, and only for secrets that actually had a "*" rule.
func TestMigrationMaterializesLegacyWildcardHosts(t *testing.T) {
	st := &State{
		Version: 9,
		Secrets: []SecretRec{
			{ID: "s1", Placeholder: "piso_legacy_1", Value: "v1"},                                  // had "*" rule
			{ID: "s2", Placeholder: "piso_never_2", Value: "v2"},                                   // no rule at all
			{ID: "s3", Placeholder: "piso_bound_3", Value: "v3", AllowedHosts: []string{"a.test"}}, // already bound
			{ID: "s4", Placeholder: "piso_gw_4", Value: "v4", GatewayOnly: true},                  // gateway-only
			{ID: "s5", Placeholder: "piso_revoked_5", Revoked: true},                              // tombstone
		},
		Rules: []RuleRec{
			{ID: "r1", SecretID: "s1", Placeholder: "piso_legacy_1", Host: "*"},
			{ID: "r3", SecretID: "s3", Placeholder: "piso_bound_3", Host: "a.test"},
			{ID: "r4", SecretID: "s4", Placeholder: "piso_gw_4", Host: "*"},
		},
	}
	if !applyMigrations(st) {
		t.Fatal("migration reported no change")
	}
	if st.Version != CurrentStateVersion {
		t.Fatalf("version = %d, want %d", st.Version, CurrentStateVersion)
	}
	if got := st.Secrets[0].AllowedHosts; len(got) != 1 || got[0] != "*" {
		t.Fatalf("legacy wildcard secret hosts = %v, want [*]", got)
	}
	for i, want := range map[int]int{1: 0, 2: 1, 3: 0, 4: 0} {
		if got := len(st.Secrets[i].AllowedHosts); got != want {
			t.Fatalf("secret %d hosts = %v, want %d entries", i, st.Secrets[i].AllowedHosts, want)
		}
	}
	if st.Secrets[2].AllowedHosts[0] != "a.test" {
		t.Fatalf("bound hosts rewritten: %v", st.Secrets[2].AllowedHosts)
	}
}
