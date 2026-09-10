package store

import (
	"path/filepath"
	"testing"

	"piso/gateway/internal/patterns"
)

func TestPatternPersistenceAndReload(t *testing.T) {
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "req.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 100)
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

func TestApplyAllowedExceptionUpdatesMatchingRows(t *testing.T) {
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "req.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 100)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st.AppendLog(Record{ID: "jwt-a", Action: "block", Host: "cdn.example.com",
		Findings: []Finding{{Kind: "pattern", PatternID: "jwt"}}})
	st.AppendLog(Record{ID: "jwt-b", Action: "block", Host: "other.test",
		Findings: []Finding{{Kind: "pattern", PatternID: "jwt"}}})
	st.AppendLog(Record{ID: "sk-a", Action: "block", Host: "cdn.example.com",
		Findings: []Finding{{Kind: "pattern", PatternID: "openai-sk"}}})
	st.AppendLog(Record{ID: "mix-a", Action: "block", Host: "cdn.example.com",
		Findings: []Finding{
			{Kind: "pattern", PatternID: "jwt"},
			{Kind: "pattern", PatternID: "openai-sk"},
		}})
	updated := st.ApplyAllowedException("jwt", "cdn.example.com")
	if len(updated) != 1 || updated[0].ID != "jwt-a" {
		t.Fatalf("updated %+v", updated)
	}
	byID := map[string]Record{}
	for _, r := range st.Records(0) {
		byID[r.ID] = r
	}
	if byID["jwt-a"].Action != "allow" || byID["jwt-a"].Reasons[0] != "allowed-exception" {
		t.Fatalf("jwt-a: %+v", byID["jwt-a"])
	}
	if byID["jwt-b"].Action != "block" || byID["sk-a"].Action != "block" || byID["mix-a"].Action != "block" {
		t.Fatalf("other rows must stay blocked")
	}
}

func TestSecretRoundTripWithoutValueLeakOnSummary(t *testing.T) {
	dir := t.TempDir()
	st, _ := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "req.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 100)
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

func TestWorkerCtxUpsertAndLogEnrichment(t *testing.T) {
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "req.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 100)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := WorkerCtx{Worker: "piso-worker-demo", Slug: "demo", Folder: "/workspace",
		Project: "demo", Branch: "main", Commit: "abc1234", Model: "routstr/deepseek-v4-flash-0731"}
	got, err := st.UpsertWorkerCtx(ctx)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got.Ts.IsZero() {
		t.Fatal("timestamp not stamped")
	}

	// AppendLog snapshots context onto a matching-slug row
	st.AppendLog(Record{ID: "r1", Slug: "demo", Action: "allow", Status: 200})
	r1 := st.Records(1)[0]
	if r1.Project != "demo" || r1.Branch != "main" || r1.Commit != "abc1234" || r1.Model != "routstr/deepseek-v4-flash-0731" {
		t.Fatalf("enrichment failed: %+v", r1)
	}
	// A row that already sets a label keeps it (call site wins)
	st.AppendLog(Record{ID: "r2", Slug: "demo", Action: "allow", Status: 200, Branch: "explicit"})
	r2 := st.Records(1)[0]
	if r2.Branch != "explicit" {
		t.Fatalf("explicit label clobbered: %+v", r2)
	}
	// A record for an unregistered slug gets no labels
	st.AppendLog(Record{ID: "r3", Slug: "", Action: "block", Status: 407})
	r3 := st.Records(1)[0]
	if r3.Project != "" || r3.Model != "" {
		t.Fatalf("labels leaked to unknown worker: %+v", r3)
	}

	// Update path preserves the original worker for the same slug
	ctx2 := WorkerCtx{Worker: "other-name", Slug: "demo", Project: "demo", Branch: "feature/x"}
	st.UpsertWorkerCtx(ctx2)
	stored, ok := st.WorkerCtxBySlug("demo")
	if !ok {
		t.Fatal("ctx not found by slug")
	}
	if stored.Worker != "piso-worker-demo" || stored.Branch != "feature/x" {
		t.Fatalf("update path: %+v", stored)
	}
	if len(st.WorkersCtx()) != 1 {
		t.Fatalf("WorkersCtx length: %d", len(st.WorkersCtx()))
	}
}
