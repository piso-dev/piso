package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"piso/gateway/internal/store"
)

// The requirement, end to end at the HTTP level: someone signs in on one
// gateway, the session is shared, and every other gateway's console reports the
// same account — same token, same scopes — without anyone signing in there.
func TestGithubSignInStatusFollowsSharedSession(t *testing.T) {
	s := testServer(t)
	// The shared session materialized by technocore on this gateway.
	if err := s.Store.AddSecret(store.SecretRec{ID: "cs_tc", Name: "GitHub OAuth client secret", Kind: "github-oauth", Placeholder: "piso_github_oauth_cs", Value: "client-secret", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AddSecret(store.SecretRec{ID: "tk_tc", Name: "GitHub user OAuth — gateway only", Kind: "github-oauth", Placeholder: "piso_github_oauth_tk", Value: "user-token-value", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetGithubUser(store.GithubUserRec{
		ClientID: "Iv1.abc", ClientSecretID: "cs_tc", TokenSecretID: "tk_tc",
		Login: "octocat", AccountID: 583231, Scopes: "read:user read:org",
		Source: store.GithubUserSourceTechnocore, TechnocoreID: "rid", ContentVersion: 4,
	}); err != nil {
		t.Fatal(err)
	}
	s.HTTP = &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" && r.Header.Get("Authorization") == "Bearer user-token-value" {
			w.Write([]byte(`{"id":583231,"login":"octocat","type":"User"}`))
			return
		}
		w.WriteHeader(401)
	})}}

	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/github-user", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["configured"] != true || out["connected"] != true {
		t.Fatalf("peer is not signed in: %v", out)
	}
	if out["login"] != "octocat" || out["source"] != store.GithubUserSourceTechnocore {
		t.Fatalf("identity: %v", out)
	}
	// The dashboard's OAuth App dialog shows which app is in use; without the
	// id it opens blank, which looks like a broken connection.
	if out["clientId"] != "Iv1.abc" {
		t.Fatalf("client id for the config dialog: %v", out)
	}
	if _, ok := out["clientSecret"]; ok {
		t.Fatalf("client secret must never be returned: %v", out)
	}
	if out["orgAccess"] != true {
		t.Fatalf("scopes not shared: %v", out)
	}
}

// Signing out anywhere signs out everywhere: the shared transport row is
// tombstoned rather than forgotten, so a peer cannot republish the session.
func TestGithubSignOutIsGlobal(t *testing.T) {
	s := testServer(t)
	signInGithubUser(t, s)
	// A configured OAuth app, so sign-out has something it must not drop.
	if err := s.Store.AddSecret(store.SecretRec{ID: "sec_client", Name: "GitHub OAuth client secret", Kind: "github-oauth", Placeholder: "piso_client", Value: "client-secret", GatewayOnly: true}); err != nil {
		t.Fatal(err)
	}
	u := s.Store.GithubUser()
	u.ClientID = "Iv1.abc"
	u.ClientSecretID = "sec_client"
	if err := s.Store.SetGithubUser(u); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureGithubUserBundle(); err != nil {
		t.Fatal(err)
	}
	// Stamp the row as published, the state after a reconcile.
	row, ok := s.githubUserBundleRow()
	if !ok {
		t.Fatal("no shared sign-in row")
	}
	row.TechnocoreID = "rid"
	row.ContentVersion = 2
	if err := s.Store.ReplaceSecret(row); err != nil {
		t.Fatal(err)
	}

	w := doJSON(t, s.ControlHandler(), "DELETE", "/api/v1/github-user", nil)
	if w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if u := s.Store.GithubUser(); u.TokenSecretID != "" || u.Login != "" || u.AccountID != 0 {
		t.Fatalf("session survived sign-out: %+v", u)
	}
	if _, ok := s.Store.SecretByID("sec_user"); ok {
		t.Fatal("token row survived sign-out")
	}
	// The transport row stops being a live secret, so reconcile can never
	// republish the session. On an enrolled gateway it is kept as a tombstone;
	// an unenrolled one is simply deleted, because it has no peers to tell.
	if _, ok := s.Store.SecretByPlaceholder(row.Placeholder); ok {
		t.Fatal("signed-out session still listed as a live secret")
	}
	for _, rec := range s.Store.Secrets() {
		if rec.Kind == "github-user" {
			t.Fatalf("shared sign-in row outlived sign-out: %+v", rec)
		}
	}
	// The OAuth app stays configured: signing back in needs no re-setup.
	if after := s.Store.GithubUser(); after.ClientID != "Iv1.abc" || after.ClientSecretID != "sec_client" {
		t.Fatalf("sign-out dropped the OAuth app configuration: %+v", after)
	}
}

// Signing in creates the transport row that makes the session shareable.
func TestGithubSignInCreatesSharedBundleRow(t *testing.T) {
	s := testServer(t)
	if _, ok := s.githubUserBundleRow(); ok {
		t.Fatal("bundle row exists before sign-in")
	}
	if err := s.ensureGithubUserBundle(); err != nil {
		t.Fatal(err)
	}
	// Idempotent: a second sign-in reuses the row instead of publishing a
	// second session.
	first, _ := s.githubUserBundleRow()
	if err := s.ensureGithubUserBundle(); err != nil {
		t.Fatal(err)
	}
	second, ok := s.githubUserBundleRow()
	if !ok || first.ID != second.ID {
		t.Fatalf("row churned: %s -> %s", first.ID, second.ID)
	}
	if second.Kind != "github-user" || !second.GatewayOnly {
		t.Fatalf("row: %+v", second)
	}
}

// Signing in on a gateway that was showing someone else's shared session takes
// the session over: it is now this gateway's own, so changes are published
// instead of being skipped as a mirror, and the existing shared row is reused
// rather than a second session being minted.
func TestGithubSignInClaimsSharedSession(t *testing.T) {
	s := testServer(t)
	if err := s.Store.AddSecret(store.SecretRec{
		ID: "tc_rid", Name: "GitHub sign-in", Kind: "github-user",
		Placeholder: "piso_github_user_peer", GatewayOnly: true, TechnocoreID: "rid",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetGithubUser(store.GithubUserRec{
		ClientID: "Iv1.abc", TokenSecretID: "tk_peer", Login: "someone-else", AccountID: 99,
		Source: store.GithubUserSourceTechnocore, TechnocoreID: "rid", ContentVersion: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureGithubUserBundle(); err != nil {
		t.Fatal(err)
	}
	u := s.Store.GithubUser()
	if u.SourceOrLocal() != store.GithubUserSourceLocal {
		t.Fatalf("session not claimed locally: %+v", u)
	}
	if u.BundleID != "tc_rid" {
		t.Fatalf("sign-in minted a second session row: %q", u.BundleID)
	}
	if n := len(s.Store.Secrets()); n != 1 {
		t.Fatalf("extra rows created: %d", n)
	}
}

// The transport row is not a vault entry: the secrets tab must not offer to
// edit or delete it.
func TestGithubSignInTransportRowHiddenFromSecrets(t *testing.T) {
	s := testServer(t)
	signInGithubUser(t, s)
	if err := s.ensureGithubUserBundle(); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/secrets", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row["name"] == "GitHub sign-in" {
			t.Fatalf("transport row listed as a secret: %v", row)
		}
	}
	// It is still in the store, where reconcile finds it.
	if _, ok := s.githubUserBundleRow(); !ok {
		t.Fatal("transport row was dropped instead of hidden")
	}
}
