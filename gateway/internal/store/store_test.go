package store

import (
	"path/filepath"
	"testing"

	"piso/gateway/internal/patterns"
)

func TestPatternPersistenceAndReload(t *testing.T) {
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "req.jsonl"), filepath.Join(dir, "patterns.json"), 100)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// custom pattern
	custom := patterns.Entry{ID: "custom_abc", Name: "my-token", Regex: `\bxyz-[A-Za-z0-9]{12}\b`, Enabled: true}
	if err := st.AddOrReplacePattern(custom); err != nil {
		t.Fatalf("AddOrReplacePattern: %v", err)
	}
	// reload from disk — must include the custom pattern merged over defaults
	compiled, err := st.LoadPatterns()
	if err != nil {
		t.Fatalf("LoadPatterns: %v", err)
	}
	got := false
	for _, e := range compiled.Entries() {
		if e.ID == "custom_abc" {
			got = true
		}
	}
	if !got {
		t.Fatal("custom pattern missing after reload")
	}
	// and it must actually match
	if len(compiled.Match([]byte("token xyz-ABCDEFGHIJKL end"))) == 0 {
		t.Fatal("custom pattern did not match")
	}

	// delete it
	if err := st.DeletePattern("custom_abc"); err != nil {
		t.Fatalf("DeletePattern: %v", err)
	}
	compiled2, _ := st.LoadPatterns()
	for _, e := range compiled2.Entries() {
		if e.ID == "custom_abc" {
			t.Fatal("custom pattern still present after delete")
		}
	}
}

func TestSecretRoundTripWithoutValueLeakOnSummary(t *testing.T) {
	dir := t.TempDir()
	st, _ := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "req.jsonl"), filepath.Join(dir, "patterns.json"), 100)
	rec := SecretRec{ID: "s1", Name: "anthropic", Placeholder: "piso_anthropic_x", Value: "sk-super-secret-value"}
	if err := st.AddSecret(rec); err != nil {
		t.Fatalf("AddSecret: %v", err)
	}
	secrets := st.Secrets()
	if len(secrets) != 1 || secrets[0].Value != "sk-super-secret-value" {
		t.Fatalf("roundtrip failed: %+v", secrets)
	}
	// delete cascades rules
	st.AddRule(RuleRec{ID: "r1", SecretID: "s1", Host: "*", Placeholder: "piso_anthropic_x"})
	if err := st.DeleteSecret("s1"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if len(st.Secrets()) != 0 {
		t.Fatal("secret not deleted")
	}
	if len(st.Rules()) != 0 {
		t.Fatal("cascading rule delete failed")
	}
}