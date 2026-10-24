package technocore

import (
	"testing"
	"time"

	"piso/gateway/internal/githubapp"
	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

// signInOrigin is a gateway where a person signed in: the OAuth client secret
// and the user token are gateway-only rows and the rec points at them.
func signInOrigin(t *testing.T, st *store.Store) {
	t.Helper()
	for _, rec := range []store.SecretRec{
		{ID: "cs_1", Name: "GitHub OAuth client secret", Kind: model.SecretKindGithubOAuth, Placeholder: "piso_github_oauth_cs", Value: "client-secret-value", GatewayOnly: true, CreatedAt: time.Now().UTC()},
		{ID: "tk_1", Name: "GitHub user OAuth — gateway only", Kind: model.SecretKindGithubOAuth, Placeholder: "piso_github_oauth_tk", Value: "gho_token_value", GatewayOnly: true, CreatedAt: time.Now().UTC()},
		{ID: "sec_bundle", Name: "GitHub sign-in", Kind: model.SecretKindGithubUser, Placeholder: "piso_github_user_abc", GatewayOnly: true, CreatedAt: time.Now().UTC()},
	} {
		if err := st.AddSecret(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetGithubUser(store.GithubUserRec{
		ClientID: "Iv1.abc", ClientSecretID: "cs_1", TokenSecretID: "tk_1",
		Login: "octocat", AccountID: 583231, Scopes: "read:user read:org",
	}); err != nil {
		t.Fatal(err)
	}
}

func userEnvelope(t *testing.T, id tc.Identity, resourceID, placeholder, value string, version int) envelope {
	t.Helper()
	obj := testEnvelope(t, id, resourceID, "GitHub sign-in", placeholder, value)
	obj.Metadata["kind"] = model.SecretKindGithubUser
	obj.Metadata["gatewayOnly"] = true
	obj.ContentVersion = version
	return obj
}

// A sign-in published by one gateway puts every other gateway's dashboard in
// the same state: same account, same token, same scopes — with the token held
// as a local gateway-only row so the existing readers work unchanged.
func TestApplyEnvelopeSharesGithubSignIn(t *testing.T) {
	st := testStore(t)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := githubapp.EncodeUserBundle(githubapp.UserBundle{
		ClientID: "Iv1.abc", ClientSecret: "client-secret-value", Token: "gho_token_value",
		Login: "octocat", AccountID: 583231, Scopes: "read:user read:org",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(st, id, userEnvelope(t, id, "rid", "piso_github_user_abc", plain, 2)); err != nil {
		t.Fatal(err)
	}
	u := st.GithubUser()
	if u.Login != "octocat" || u.AccountID != 583231 || u.ClientID != "Iv1.abc" {
		t.Fatalf("identity: %+v", u)
	}
	if u.SourceOrLocal() != store.GithubUserSourceTechnocore || u.TechnocoreID != "rid" || u.ContentVersion != 2 {
		t.Fatalf("provenance: %+v", u)
	}
	if u.Scopes != "read:user read:org" {
		t.Fatalf("scopes: %q", u.Scopes)
	}
	// The token is a real gateway-only row, or nothing downstream can use it.
	tok, ok := st.SecretByID(u.TokenSecretID)
	if !ok || tok.Value != "gho_token_value" || !tok.GatewayOnly || tok.Kind != model.SecretKindGithubOAuth {
		t.Fatalf("token row: %+v", tok)
	}
	// The transport row itself holds no credential.
	bundle, ok := st.SecretByID("tc_rid")
	if !ok || bundle.Value != "" || bundle.Kind != model.SecretKindGithubUser {
		t.Fatalf("bundle row leaked: %+v", bundle)
	}
	// A resync must not accumulate copies of the token.
	if _, err := applyEnvelope(st, id, userEnvelope(t, id, "rid", "piso_github_user_abc", plain, 3)); err != nil {
		t.Fatal(err)
	}
	if again := st.GithubUser(); again.TokenSecretID != u.TokenSecretID {
		t.Fatalf("token row replaced: %s -> %s", u.TokenSecretID, again.TokenSecretID)
	}
	if n := countOAuthRows(t, st); n != 2 {
		t.Fatalf("oauth rows: %d, want 2", n)
	}
}

// Signing out is global: the origin's tombstone clears the peer's session, and
// the peer stops being able to act as that account.
func TestApplyEnvelopeRevokesSharedGithubSignIn(t *testing.T) {
	st := testStore(t)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := githubapp.EncodeUserBundle(githubapp.UserBundle{Token: "gho_token_value", Login: "octocat", AccountID: 583231})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(st, id, userEnvelope(t, id, "rid", "piso_github_user_abc", plain, 2)); err != nil {
		t.Fatal(err)
	}
	obj := userEnvelope(t, id, "rid", "piso_github_user_abc", plain, 3)
	obj.Status = statusRevoked
	if _, err := applyEnvelope(st, id, obj); err != nil {
		t.Fatal(err)
	}
	u := st.GithubUser()
	if u.TokenSecretID != "" || u.Login != "" || u.AccountID != 0 {
		t.Fatalf("session survived revocation: %+v", u)
	}
	for _, rec := range st.Secrets() {
		if rec.Kind == model.SecretKindGithubOAuth && rec.Name == "GitHub user OAuth — gateway only" && rec.Value == "gho_token_value" {
			t.Fatal("revoked token left behind")
		}
	}
}

// A gateway that signed in itself is not signed out by a peer's tombstone, and
// an inbound sign-in never replaces a local one.
func TestSharedGithubSignInLocalWins(t *testing.T) {
	st := testStore(t)
	signInOrigin(t, st) // this gateway is signed in as octocat
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	other, err := githubapp.EncodeUserBundle(githubapp.UserBundle{Token: "gho_peer", Login: "someone-else", AccountID: 99})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(st, id, userEnvelope(t, id, "rid", "piso_github_user_peer", other, 5)); err != nil {
		t.Fatal(err)
	}
	u := st.GithubUser()
	if u.Login != "octocat" || u.TokenSecretID != "tk_1" || u.SourceOrLocal() != store.GithubUserSourceLocal {
		t.Fatalf("local sign-in was overwritten: %+v", u)
	}
	obj := userEnvelope(t, id, "rid", "piso_github_user_peer", other, 6)
	obj.Status = statusRevoked
	if _, err := applyEnvelope(st, id, obj); err != nil {
		t.Fatal(err)
	}
	if u := st.GithubUser(); u.Login != "octocat" || u.TokenSecretID != "tk_1" {
		t.Fatalf("peer tombstone signed this gateway out: %+v", u)
	}
}

// Only the gateway where the person signed in publishes the session, and only
// while it has a token. The two halves of the sign-in never travel alone.
func TestSharePlaintextGithubSignIn(t *testing.T) {
	st := testStore(t)
	signInOrigin(t, st)
	plain, ok := sharePlaintext(st, mustSecret(t, st, "sec_bundle"))
	if !ok {
		t.Fatal("signed-in gateway did not share its session")
	}
	bundle, err := githubapp.DecodeUserBundle(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Login != "octocat" || bundle.Token != "gho_token_value" || bundle.ClientSecret != "client-secret-value" || bundle.ClientID != "Iv1.abc" {
		t.Fatalf("bundle: %+v", bundle)
	}
	// The materialized halves are internal to the bundle.
	for _, id := range []string{"cs_1", "tk_1"} {
		if _, ok := sharePlaintext(st, mustSecret(t, st, id)); ok {
			t.Fatalf("%s published on its own", id)
		}
	}
	// A mirror is never republished.
	mirror := testStore(t)
	if err := mirror.AddSecret(mustSecret(t, st, "sec_bundle")); err != nil {
		t.Fatal(err)
	}
	if err := mirror.SetGithubUser(store.GithubUserRec{ClientID: "x", TokenSecretID: "tk", Login: "octocat", AccountID: 1, Source: store.GithubUserSourceTechnocore}); err != nil {
		t.Fatal(err)
	}
	if _, ok := sharePlaintext(mirror, mustSecret(t, mirror, "sec_bundle")); ok {
		t.Fatal("mirrored sign-in was republished")
	}
	// Signed out: nothing to share.
	if err := st.SetGithubUser(store.GithubUserRec{ClientID: "Iv1.abc", ClientSecretID: "cs_1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := sharePlaintext(st, mustSecret(t, st, "sec_bundle")); ok {
		t.Fatal("signed-out gateway published a session")
	}
}

func countOAuthRows(t *testing.T, st *store.Store) int {
	t.Helper()
	n := 0
	for _, rec := range st.Secrets() {
		if rec.Kind == model.SecretKindGithubOAuth {
			n++
		}
	}
	return n
}

func mustSecret(t *testing.T, st *store.Store, id string) store.SecretRec {
	t.Helper()
	rec, ok := st.SecretByID(id)
	if !ok {
		t.Fatalf("secret %s missing", id)
	}
	return rec
}

// One gateway can hold its own sign-in and a peer's session at the same time.
// Sharing is decided per row, so the peer's row is never republished with this
// gateway's token (which would silently sign every other gateway in as the
// wrong person).
func TestSharePlaintextSkipsMirroredRowWhenAlsoSignedIn(t *testing.T) {
	st := testStore(t)
	signInOrigin(t, st)
	// The mirrored session from a peer, alongside the local one.
	if err := st.AddSecret(store.SecretRec{
		ID: "tc_peer", Name: "GitHub sign-in", Kind: model.SecretKindGithubUser,
		Placeholder: "piso_github_user_peer", GatewayOnly: true,
		TechnocoreID: "peer-rid", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	u := st.GithubUser()
	u.BundleID = "sec_bundle" // the row this gateway created on sign-in
	if err := st.SetGithubUser(u); err != nil {
		t.Fatal(err)
	}
	if _, ok := sharePlaintext(st, mustSecret(t, st, "sec_bundle")); !ok {
		t.Fatal("own session not shared")
	}
	if _, ok := sharePlaintext(st, mustSecret(t, st, "tc_peer")); ok {
		t.Fatal("peer's mirrored session republished with this gateway's token")
	}
}

// A peer drops the mirrored transport row along with the session, so a later
// sign-in there creates a fresh row instead of republishing into a resource the
// coordinator already tombstoned.
func TestRevokedSharedSignInDropsTransportRow(t *testing.T) {
	st := testStore(t)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := githubapp.EncodeUserBundle(githubapp.UserBundle{Token: "gho_token_value", Login: "octocat", AccountID: 583231})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(st, id, userEnvelope(t, id, "rid", "piso_github_user_abc", plain, 2)); err != nil {
		t.Fatal(err)
	}
	obj := userEnvelope(t, id, "rid", "piso_github_user_abc", plain, 3)
	obj.Status = statusRevoked
	if _, err := applyEnvelope(st, id, obj); err != nil {
		t.Fatal(err)
	}
	for _, rec := range st.Secrets() {
		if rec.Kind == model.SecretKindGithubUser || rec.Kind == model.SecretKindGithubOAuth {
			t.Fatalf("row survived revocation: %+v", rec)
		}
	}
}
