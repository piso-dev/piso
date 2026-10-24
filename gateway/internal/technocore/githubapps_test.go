package technocore

import (
	"testing"
	"time"

	"piso/gateway/internal/githubapp"
	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

func appEnvelope(t *testing.T, id tc.Identity, resourceID, placeholder string, value string, version int) envelope {
	t.Helper()
	obj := testEnvelope(t, id, resourceID, "GitHub", placeholder, value)
	obj.Metadata["kind"] = model.SecretKindGithubApp
	obj.Metadata["envKey"] = "GH_TOKEN"
	obj.ContentVersion = version
	return obj
}

func seedAppGroup(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.AddSecret(store.SecretRec{
		ID: "tc_rid", Name: "GitHub", Kind: model.SecretKindGithubApp,
		Placeholder: "piso_github_abc", EnvKey: "GH_TOKEN",
		AllowedHosts: []string{"api.github.com"}, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func sharedApp(t *testing.T) string {
	t.Helper()
	plain, err := githubapp.EncodeBundle([]store.GithubAppRec{{
		ID: "gha_origin", SecretID: "sec_origin", AppID: 42, Name: "piso-origin", Slug: "piso-origin",
		PrivateKey:    "-----BEGIN RSA PRIVATE KEY-----\nkey\n-----END RSA PRIVATE KEY-----",
		Installations: []store.GithubInstallation{{ID: 99, Account: "octocat", Repos: []string{"octocat/only-repo"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

// A routing group arrives from a peer as a bundle: the vault row stays empty
// (workers must never see the PEM) and the apps are materialised as read-only
// mirrors that can mint.
func TestApplyEnvelopeMirrorsSharedGithubApp(t *testing.T) {
	st := testStore(t)
	seedAppGroup(t, st)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(st, id, appEnvelope(t, id, "rid", "piso_github_abc", sharedApp(t), 3)); err != nil {
		t.Fatal(err)
	}
	sec, ok := st.SecretByID("tc_rid")
	if !ok {
		t.Fatal("no group secret")
	}
	if sec.Value != "" {
		t.Fatalf("bundle leaked into the vault: %q", sec.Value)
	}
	if sec.Kind != model.SecretKindGithubApp {
		t.Fatalf("kind: %q", sec.Kind)
	}
	app, ok := st.GithubAppByAppID(42)
	if !ok {
		t.Fatal("app not mirrored")
	}
	if !app.Mirrored() || app.SecretID != "tc_rid" || app.PrivateKey == "" {
		t.Fatalf("mirror: %+v", app)
	}
	if app.Status() != store.GithubAppStatusOK {
		t.Fatalf("status: %s", app.Status())
	}
	if len(app.Installations) != 1 || app.Installations[0].Account != "octocat" {
		t.Fatalf("installations: %+v", app.Installations)
	}
	// The mirrored placeholder must be routable: policy needs a PEM-backed app
	// under the group's placeholder.
	views := st.GithubAppViews()
	if v, ok := views["piso_github_abc"]; !ok || !v.HasPEM {
		t.Fatalf("policy view: %+v", views)
	}
	if !st.GithubGroupManagedByPeer("tc_rid") {
		t.Fatal("group not marked peer-managed")
	}
}

// Resyncing the same app updates one row instead of accumulating duplicates.
func TestApplyEnvelopeUpdatesMirrorInPlace(t *testing.T) {
	st := testStore(t)
	seedAppGroup(t, st)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(st, id, appEnvelope(t, id, "rid", "piso_github_abc", sharedApp(t), 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(st, id, appEnvelope(t, id, "rid", "piso_github_abc", sharedApp(t), 4)); err != nil {
		t.Fatal(err)
	}
	apps := st.GithubAppsBySecretID("tc_rid")
	if len(apps) != 1 {
		t.Fatalf("duplicated mirrors: %+v", apps)
	}
	if apps[0].ContentVersion != 4 {
		t.Fatalf("version: %d", apps[0].ContentVersion)
	}
}

// An app configured on this gateway is never replaced by a peer's copy.
func TestApplyEnvelopeKeepsLocalApp(t *testing.T) {
	st := testStore(t)
	seedAppGroup(t, st)
	if _, err := st.UpsertGithubApp(store.GithubAppRec{
		ID: "gha_local", SecretID: "tc_rid", AppID: 42, Name: "local", PrivateKey: "local-pem",
	}); err != nil {
		t.Fatal(err)
	}
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(st, id, appEnvelope(t, id, "rid", "piso_github_abc", sharedApp(t), 3)); err != nil {
		t.Fatal(err)
	}
	app, _ := st.GithubAppByAppID(42)
	if app.Mirrored() || app.PrivateKey != "local-pem" {
		t.Fatalf("local app overwritten: %+v", app)
	}
}

// A revoked group must drop its mirrors, or peers keep minting against a
// deleted app until the first 401.
func TestApplyEnvelopeRevokedGithubAppDropsMirrors(t *testing.T) {
	st := testStore(t)
	seedAppGroup(t, st)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(st, id, appEnvelope(t, id, "rid", "piso_github_abc", sharedApp(t), 3)); err != nil {
		t.Fatal(err)
	}
	revoked := appEnvelope(t, id, "rid", "piso_github_abc", sharedApp(t), 5)
	revoked.Status = statusRevoked
	if _, err := applyEnvelope(st, id, revoked); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GithubAppByAppID(42); ok {
		t.Fatal("mirror survived revocation")
	}
}

// A plaintext that is not a bundle (an older gateway's PAT, say) must not be
// imported as zero apps, and must not take the vault row down with it.
func TestApplyEnvelopeIgnoresNonBundleGithubSecret(t *testing.T) {
	st := testStore(t)
	seedAppGroup(t, st)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(st, id, appEnvelope(t, id, "rid", "piso_github_abc", "ghp_plaintext", 1)); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.SecretByID("tc_rid"); !ok {
		t.Fatal("vault row dropped")
	}
	if apps := st.GithubAppsBySecretID("tc_rid"); len(apps) != 0 {
		t.Fatalf("imported from a non-bundle: %+v", apps)
	}
}

// Peer groups are never republished: the origin gateway owns them.
func TestSharePlaintextSkipsMirroredGroup(t *testing.T) {
	st := testStore(t)
	seedAppGroup(t, st)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(st, id, appEnvelope(t, id, "rid", "piso_github_abc", sharedApp(t), 3)); err != nil {
		t.Fatal(err)
	}
	sec, _ := st.SecretByID("tc_rid")
	if _, ok := sharePlaintext(st, sec); ok {
		t.Fatal("republished a mirrored group")
	}
}

// The origin gateway publishes its own group as a bundle even though the vault
// row has no value.
func TestSharePlaintextEncodesLocalGroup(t *testing.T) {
	st := testStore(t)
	if err := st.AddSecret(store.SecretRec{
		ID: "sec_local", Name: "GitHub", Kind: model.SecretKindGithubApp,
		Placeholder: "piso_github_local", EnvKey: "GH_TOKEN",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertGithubApp(store.GithubAppRec{
		ID: "gha_local", SecretID: "sec_local", AppID: 42, Slug: "piso-local", PrivateKey: "pem",
		Installations: []store.GithubInstallation{{ID: 7, Account: "octocat"}},
	}); err != nil {
		t.Fatal(err)
	}
	sec, _ := st.SecretByID("sec_local")
	plain, ok := sharePlaintext(st, sec)
	if !ok {
		t.Fatal("local group not published")
	}
	b, err := githubapp.DecodeBundle(plain)
	if err != nil || len(b.Apps) != 1 || b.Apps[0].AppID != 42 {
		t.Fatalf("bundle: %+v %v", b, err)
	}
}
