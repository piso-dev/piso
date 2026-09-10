package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderPlaceholdersEnvOmitsValues(t *testing.T) {
	body := RenderPlaceholdersEnv([]SecretRec{
		{EnvKey: "ANTHROPIC_API_KEY", Placeholder: "piso_anthropic_abc", Value: "sk-SUPER-SECRET"},
		{EnvKey: "OPENAI_API_KEY", Placeholder: "piso_openai_xyz", Value: "sk-also-secret"},
		{Name: "no-env", Placeholder: "piso_x", Value: "leak-me"},
		{EnvKey: "9BAD", Placeholder: "piso_bad", Value: "nope"},
	})
	if strings.Contains(body, "sk-") || strings.Contains(body, "SECRET") || strings.Contains(body, "leak") {
		t.Fatalf("value leaked into env file:\n%s", body)
	}
	if !strings.Contains(body, "ANTHROPIC_API_KEY=piso_anthropic_abc") {
		t.Fatalf("missing anthropic line:\n%s", body)
	}
	if !strings.Contains(body, "OPENAI_API_KEY=piso_openai_xyz") {
		t.Fatalf("missing openai line:\n%s", body)
	}
	if strings.Contains(body, "9BAD") {
		t.Fatal("invalid env key should be skipped")
	}
}

func TestWritePlaceholdersEnv(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	if err := WritePlaceholdersEnv(state, []SecretRec{
		{EnvKey: "FOO_KEY", Placeholder: "piso_foo_1", Value: "real-must-not-appear"},
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, PlaceholdersEnvName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "real-must-not-appear") {
		t.Fatal("value written to disk")
	}
	if !strings.Contains(string(b), "FOO_KEY=piso_foo_1") {
		t.Fatalf("got %s", b)
	}
}

func TestSecretVisibleToWorker(t *testing.T) {
	all := SecretRec{EnvKey: "A_KEY", Placeholder: "piso_a"}
	if !SecretVisibleToWorker(all, "proj-a") || !SecretVisibleToWorker(all, "proj-b") {
		t.Fatal("empty workers should be visible to all")
	}
	star := SecretRec{EnvKey: "A_KEY", Placeholder: "piso_a", Workers: []string{"*"}}
	if !SecretVisibleToWorker(star, "proj-b") {
		t.Fatal("* should be visible to all")
	}
	scoped := SecretRec{EnvKey: "A_KEY", Placeholder: "piso_a", Workers: []string{"proj-a"}}
	if !SecretVisibleToWorker(scoped, "proj-a") || SecretVisibleToWorker(scoped, "proj-b") {
		t.Fatal("scoped visibility")
	}
	if SecretVisibleToWorker(SecretRec{Placeholder: "piso_a"}, "proj-a") {
		t.Fatal("missing env key should be hidden")
	}
}

func TestWriteAllWorkerEnvsFiltersScope(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	if err := os.MkdirAll(filepath.Join(dir, "workers", "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "workers", "beta"), 0o700); err != nil {
		t.Fatal(err)
	}
	secrets := []SecretRec{
		{EnvKey: "SHARED_KEY", Placeholder: "piso_shared", Value: "REAL-SHARED"},
		{EnvKey: "ALPHA_ONLY", Placeholder: "piso_alpha", Value: "REAL-ALPHA", Workers: []string{"alpha"}},
	}
	if err := WriteAllWorkerEnvs(state, secrets); err != nil {
		t.Fatal(err)
	}
	a, err := os.ReadFile(filepath.Join(dir, "workers", "alpha", PlaceholdersEnvName))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "workers", "beta", PlaceholdersEnvName))
	if err != nil {
		t.Fatal(err)
	}
	as, bs := string(a), string(b)
	if strings.Contains(as, "REAL") || strings.Contains(bs, "REAL") {
		t.Fatal("value leaked")
	}
	if !strings.Contains(as, "SHARED_KEY=piso_shared") || !strings.Contains(as, "ALPHA_ONLY=piso_alpha") {
		t.Fatalf("alpha file: %s", as)
	}
	if !strings.Contains(bs, "SHARED_KEY=piso_shared") {
		t.Fatalf("beta missing shared: %s", bs)
	}
	if strings.Contains(bs, "ALPHA_ONLY") {
		t.Fatalf("beta should not see alpha-only: %s", bs)
	}
}

func TestDeriveEnvKey(t *testing.T) {
	if got := DeriveEnvKey("anthropic"); got != "ANTHROPIC" {
		t.Fatalf("basic: %q", got)
	}
	if got := DeriveEnvKey("CLD2 SSH"); got != "CLD2_SSH" {
		t.Fatalf("spaces: %q", got)
	}
	if got := DeriveEnvKey("my-key-1"); got != "MY_KEY_1" {
		t.Fatalf("dash: %q", got)
	}
	if got := DeriveEnvKey("not_"); got != "NOT" {
		t.Fatalf("edge trim: %q", got)
	}
	if got := DeriveEnvKey("`` ``"); got != "" {
		t.Fatalf("all-symbols should be empty: %q", got)
	}
	if got := DeriveEnvKey("1abc"); got != "_1ABC" {
		t.Fatalf("leading digit: %q", got)
	}
	if !ValidEnvKey(DeriveEnvKey("CLD2 SSH")) {
		t.Fatal("derived key must be valid")
	}
}

func TestValidEnvKey(t *testing.T) {
	if !ValidEnvKey("ANTHROPIC_API_KEY") || !ValidEnvKey("_X") {
		t.Fatal("expected valid")
	}
	if ValidEnvKey("") || ValidEnvKey("1A") || ValidEnvKey("A-B") || ValidEnvKey("A B") {
		t.Fatal("expected invalid")
	}
}
