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
	plain, err := githubapp.EncodeUserBundle(githubapp.UserBundle{
		ClientID: "Iv1.abc", ClientSecret: "client-secret-value",
		Token: "gho_token_value", Login: "octocat", AccountID: 583231,
	})
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

// The session is one session: a newer version from another gateway replaces the
// one signed in here, and a tombstone from another gateway signs this one out.
// There is no local copy to protect — signing in anywhere sets the account
// everywhere.
func TestSharedGithubSignInNewestVersionWins(t *testing.T) {
	st := testStore(t)
	signInOrigin(t, st) // this gateway is signed in as octocat at version 0
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
	if u.Login != "someone-else" || u.AccountID != 99 || u.ContentVersion != 5 {
		t.Fatalf("newer shared sign-in did not apply: %+v", u)
	}
	if tok, ok := st.SecretByID(u.TokenSecretID); !ok || tok.Value != "gho_peer" {
		t.Fatalf("token not materialized: %+v", tok)
	}
	// An older version never rolls it back.
	if _, err := applyEnvelope(st, id, userEnvelope(t, id, "rid", "piso_github_user_peer", other, 4)); err != nil {
		t.Fatal(err)
	}
	if u := st.GithubUser(); u.ContentVersion != 5 || u.Login != "someone-else" {
		t.Fatalf("older version rolled the session back: %+v", u)
	}
	obj := userEnvelope(t, id, "rid", "piso_github_user_peer", other, 6)
	obj.Status = statusRevoked
	if _, err := applyEnvelope(st, id, obj); err != nil {
		t.Fatal(err)
	}
	if u := st.GithubUser(); u.TokenSecretID != "" || u.Login != "" {
		t.Fatalf("tombstone did not sign this gateway out: %+v", u)
	}
}

// Any gateway holding a session publishes it, wherever the session came from:
// the two halves of a sign-in never travel alone, and being signed in on this
// gateway rather than another is provenance, not a permission.
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
	// A session received from a peer is republished like any other, so a
	// gateway that never signed in can still carry the latest version.
	received := testStore(t)
	if err := received.AddSecret(mustSecret(t, st, "sec_bundle")); err != nil {
		t.Fatal(err)
	}
	if err := received.SetGithubUser(store.GithubUserRec{ClientID: "Iv1.abc", ClientSecretID: "cs_1", TokenSecretID: "tk_1", Login: "octocat", AccountID: 1, Source: store.GithubUserSourceTechnocore}); err != nil {
		t.Fatal(err)
	}
	if err := received.AddSecret(store.SecretRec{ID: "tk_1", Kind: model.SecretKindGithubOAuth, GatewayOnly: true, Value: "gho_token_value"}); err != nil {
		t.Fatal(err)
	}
	if err := received.AddSecret(store.SecretRec{ID: "cs_1", Kind: model.SecretKindGithubOAuth, GatewayOnly: true, Value: "client-secret-value"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := sharePlaintext(received, mustSecret(t, received, "sec_bundle")); !ok {
		t.Fatal("received session was not republished")
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

// A gateway can hold more than one sign-in row (its own and one received from a
// peer) after a rebase. Either publishes the same session, so which row is
// replayed does not matter.
func TestSharePlaintextPublishesSessionFromAnyRow(t *testing.T) {
	st := testStore(t)
	signInOrigin(t, st)
	if err := st.AddSecret(store.SecretRec{
		ID: "tc_peer", Name: "GitHub sign-in", Kind: model.SecretKindGithubUser,
		Placeholder: "piso_github_user_peer", GatewayOnly: true,
		TechnocoreID: "peer-rid", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sec_bundle", "tc_peer"} {
		plain, ok := sharePlaintext(st, mustSecret(t, st, id))
		if !ok {
			t.Fatalf("%s did not publish the session", id)
		}
		bundle, err := githubapp.DecodeUserBundle(plain)
		if err != nil || bundle.Token != "gho_token_value" {
			t.Fatalf("%s bundle: %+v %v", id, bundle, err)
		}
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
	plain, err := githubapp.EncodeUserBundle(githubapp.UserBundle{
		ClientID: "Iv1.abc", ClientSecret: "client-secret-value",
		Token: "gho_token_value", Login: "octocat", AccountID: 583231,
	})
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
		if rec.Kind == model.SecretKindGithubUser || rec.Name == "GitHub user OAuth — gateway only" {
			t.Fatalf("row survived revocation: %+v", rec)
		}
	}
	// The OAuth App configuration survives, so signing back in needs no
	// re-setup on any gateway.
	if u := st.GithubUser(); u.ClientID == "" || u.ClientSecretID == "" {
		t.Fatalf("sign-out dropped the OAuth App configuration: %+v", u)
	}
}

// A gateway that signed in before sign-ins were shared holds a session and no
// transport row, so there was nothing to publish and every peer stayed signed
// out. Reconciling adopts the session once; after that it travels like any other
// secret and a second pass must not mint a rival row.
func TestReconcileAdoptsUnsharedGithubSignIn(t *testing.T) {
	st := testStore(t)
	for _, rec := range []store.SecretRec{
		{ID: "cs_1", Name: "GitHub OAuth client secret", Kind: model.SecretKindGithubOAuth, Placeholder: "piso_github_oauth_cs", Value: "client-secret-value", GatewayOnly: true, CreatedAt: time.Now().UTC()},
		{ID: "tk_1", Name: "GitHub user OAuth — gateway only", Kind: model.SecretKindGithubOAuth, Placeholder: "piso_github_oauth_tk", Value: "gho_token_value", GatewayOnly: true, CreatedAt: time.Now().UTC()},
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
	adoptLocalGithubSignIn(st)
	u := st.GithubUser()
	if u.BundleID == "" {
		t.Fatal("session was not adopted")
	}
	if u.SourceOrLocal() != store.GithubUserSourceLocal {
		t.Fatalf("adoption is not a mirrored session: %q", u.Source)
	}
	rec, ok := st.SecretByID(u.BundleID)
	if !ok || rec.Kind != model.SecretKindGithubUser || rec.Value != "" || !rec.GatewayOnly {
		t.Fatalf("transport row: %+v", rec)
	}
	// The adopted row carries the session, so reconcile has something to publish.
	plain, ok := sharePlaintext(st, rec)
	if !ok {
		t.Fatal("adopted session is not shareable")
	}
	bundle, err := githubapp.DecodeUserBundle(plain)
	if err != nil || bundle.Login != "octocat" || bundle.Token != "gho_token_value" || bundle.ClientID != "Iv1.abc" {
		t.Fatalf("bundle: %+v %v", bundle, err)
	}
	// Idempotent: a second pass reuses the row instead of publishing a rival.
	adoptLocalGithubSignIn(st)
	if again := st.GithubUser(); again.BundleID != u.BundleID {
		t.Fatalf("second adoption moved the row: %s -> %s", u.BundleID, again.BundleID)
	}
	n := 0
	for _, r := range st.Secrets() {
		if r.Kind == model.SecretKindGithubUser {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("transport rows: %d, want 1", n)
	}
}

// A gateway that received a session must not adopt a second one: the peer's row
// already exists, and minting another would publish a session it does not own.
func TestAdoptLeavesMirroredAndEmptySessionsAlone(t *testing.T) {
	st := testStore(t)
	// Signed out: no token, nothing to share.
	if err := st.SetGithubUser(store.GithubUserRec{ClientID: "Iv1.abc", ClientSecretID: "cs_1"}); err != nil {
		t.Fatal(err)
	}
	adoptLocalGithubSignIn(st)
	for _, rec := range st.Secrets() {
		if rec.Kind == model.SecretKindGithubUser {
			t.Fatalf("adopted a signed-out session: %+v", rec)
		}
	}
	// Mirrored: the envelope already arrived in a row of its own.
	if err := st.AddSecret(store.SecretRec{ID: "tk_1", Name: "GitHub user OAuth — gateway only", Kind: model.SecretKindGithubOAuth, Placeholder: "piso_tk", Value: "gho_peer", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetGithubUser(store.GithubUserRec{
		ClientID: "Iv1.abc", ClientSecretID: "cs_1", TokenSecretID: "tk_1",
		Login: "someone-else", AccountID: 99, Source: store.GithubUserSourceTechnocore, TechnocoreID: "rid", ContentVersion: 3,
	}); err != nil {
		t.Fatal(err)
	}
	adoptLocalGithubSignIn(st)
	for _, rec := range st.Secrets() {
		if rec.Kind == model.SecretKindGithubUser {
			t.Fatalf("adopted a mirrored session: %+v", rec)
		}
	}
}
