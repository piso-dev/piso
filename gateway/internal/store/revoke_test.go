package store

import (
	"path/filepath"
	"testing"
	"time"
)

// A coordinator tombstone has to keep the resource id and version after the
// plaintext is gone, otherwise a peer's stale republish has nothing to lose
// against and the secret simply comes back.
func TestRevokeSecretKeepsTombstone(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "state.json"), "", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(SecretRec{
		ID:           "tc_rid",
		Name:         "GitHub PAT",
		Placeholder:  "piso_ghp",
		Value:        "ghp_secret",
		TechnocoreID: "rid",
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncRulesForSecret(SecretRec{ID: "tc_rid", Placeholder: "piso_ghp", AllowedHosts: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	if len(st.Rules()) == 0 {
		t.Fatal("expected a rule to exist before revocation")
	}

	if err := st.RevokeSecret("tc_rid", 4); err != nil {
		t.Fatal(err)
	}

	got, ok := st.SecretByID("tc_rid")
	if !ok {
		t.Fatal("tombstone was dropped, so the version is forgotten")
	}
	if !got.Revoked || got.Value != "" || got.ContentVersion != 4 || got.TechnocoreID != "rid" {
		t.Fatalf("tombstone %+v", got)
	}
	if recs := st.Secrets(); len(recs) != 0 {
		t.Fatalf("revoked secret is still live: %+v", recs)
	}
	if _, ok := st.SecretByPlaceholder("piso_ghp"); !ok {
		t.Fatal("placeholder lookup must still find the tombstone")
	}
	if rules := st.Rules(); len(rules) != 0 {
		t.Fatalf("revoked secret kept substitution rules: %+v", rules)
	}

	// Reloading from disk preserves the tombstone across a restart.
	reloaded, err := New(st.Path(), "", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if recs := reloaded.Secrets(); len(recs) != 0 {
		t.Fatalf("revocation lost on reload: %+v", recs)
	}
	back, ok := reloaded.SecretByID("tc_rid")
	if !ok || !back.Revoked || back.ContentVersion != 4 {
		t.Fatalf("reloaded tombstone %+v ok=%v", back, ok)
	}
}
