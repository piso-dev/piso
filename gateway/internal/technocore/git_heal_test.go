package technocore

import (
	"testing"

	"piso/gateway/internal/gitaccess"
	"piso/gateway/internal/store"
)

// A gateway that predates sealed Git bundles imports the envelope's kind
// metadata but stores the raw token as Value, losing the connection config.
// GET /api/v1/git filters on Git != nil, so the row exists but the dashboard
// omits it. Healing cannot recover the lost config; verify the row is left
// untouched rather than guessed at.
func TestHealLeavesPlainTokenGitRowsUntouched(t *testing.T) {
	st := testStore(t)
	rec := store.SecretRec{
		ID: "sec_legacy", Name: "Gitea", Kind: gitaccess.Kind, Value: "raw-legacy-token",
		Placeholder: "piso_git_legacy", EnvKey: "TEA_TOKEN",
	}
	if err := st.AddSecret(rec); err != nil {
		t.Fatal(err)
	}
	healGitBundles(st)
	got, ok := st.SecretByID("sec_legacy")
	if !ok {
		t.Fatal("legacy row disappeared")
	}
	if got.Git != nil || got.Value != "raw-legacy-token" {
		t.Fatalf("heal mutated a plain-token row: %+v", got)
	}
}

// The stuck row's Value may instead be the sealed bundle JSON that the
// origin gateway published but a buggy sync stored without decoding. That
// heals in place: config decoded, token extracted, workers restored.
func TestHealDecodesStoredBundle(t *testing.T) {
	st := testStore(t)
	config := gitaccess.Config{Provider: "gitea", TokenType: "user", BaseURL: "https://git.example.test", Hints: map[string]string{"merge": "denied"}}
	sealed, err := gitaccess.Encode(config, "real-gitea-token", []string{"demo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{
		ID: "sec_stuck", Name: "Gitea", Kind: gitaccess.Kind, Value: sealed,
		Placeholder: "piso_git_stuck", EnvKey: "TEA_TOKEN",
		AllowedHosts: []string{"git.example.test"}, Workers: []string{"demo"},
	}); err != nil {
		t.Fatal(err)
	}
	healGitBundles(st)
	got, ok := st.SecretByID("sec_stuck")
	if !ok {
		t.Fatal("stuck row disappeared")
	}
	if got.Git == nil || got.Git.BaseURL != config.BaseURL || got.Git.Hints["merge"] != "denied" {
		t.Fatalf("config not materialized: %+v", got.Git)
	}
	if got.Value != "real-gitea-token" {
		t.Fatalf("token = %q, want extracted plaintext", got.Value)
	}
	if len(got.Workers) != 1 || got.Workers[0] != "demo" {
		t.Fatalf("workers = %v, want [demo]", got.Workers)
	}
	if len(got.AllowedHosts) != 1 || got.AllowedHosts[0] != "git.example.test" {
		t.Fatalf("hosts = %v, want normalized sealed hosts", got.AllowedHosts)
	}
	// Healthy and revoked rows must not be touched.
	if err := st.AddSecret(store.SecretRec{
		ID: "sec_fine", Name: "Fine", Kind: gitaccess.Kind, Git: &config,
		Value: "plain", Placeholder: "piso_git_fine", EnvKey: "OTHER_KEY",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeSecret("sec_fine", 0); err != nil {
		t.Fatal(err)
	}
	before, _ := st.SecretByID("sec_fine")
	healGitBundles(st)
	after, _ := st.SecretByID("sec_fine")
	if before.Value != after.Value {
		t.Fatal("revoked row was rewritten")
	}
}

// Healing runs inside the reconcile pass without disturbing the vault. The
// sealed bytes re-encode identically, so the publish loop's hash comparison
// still skips the row: healing is per-gateway self-repair, not a republish,
// and must not bounce content_version between peers.
func TestHealThenPublishSendsHealedBundle(t *testing.T) {
	st := testStore(t)
	config := gitaccess.Config{Provider: "gitea", TokenType: "user", BaseURL: "https://git.example.test", Hints: map[string]string{}}
	sealed, err := gitaccess.Encode(config, "real-gitea-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(store.SecretRec{
		ID: "sec_stuck", Name: "Gitea", Kind: gitaccess.Kind, Value: sealed,
		Placeholder: "piso_git_stuck", EnvKey: "TEA_TOKEN", ContentVersion: 3,
		PublishedHash: plaintextHash(sealed),
	}); err != nil {
		t.Fatal(err)
	}
	healGitBundles(st)
	got, ok := st.SecretByID("sec_stuck")
	if !ok || got.Git == nil {
		t.Fatal("row not healed")
	}
	if got.Value != "real-gitea-token" {
		t.Fatalf("substituted value = %q, want the raw token", got.Value)
	}
	plain, ok := sharePlaintext(st, got)
	if !ok {
		t.Fatal("healed row is not publishable")
	}
	if plain != sealed {
		t.Fatalf("healing changed sealed bytes; peers would replay or conflict")
	}
	if rec, _ := st.SecretByID("sec_stuck"); rec.PublishedHash == plaintextHash(plain) && rec.Git != nil {
		// Publish-loop skip condition holds: no republish, no version churn.
		return
	}
	t.Fatal("healed row would be republished, bumping versions on every peer")
}
