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
// (workers must never see the PEM) and the apps are materialised locally so
// they can mint.
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
	if app.Source != store.GithubAppSourceTechnocore || app.SecretID != "tc_rid" || app.PrivateKey == "" {
		t.Fatalf("applied app: %+v", app)
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

// The envelope is authoritative: an app edited on another gateway replaces this
// gateway's copy, and keeps this gateway's row id so links to it stay valid.
func TestApplyEnvelopeOverwritesLocalAppInPlace(t *testing.T) {
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
	app, ok := st.GithubAppByAppID(42)
	if !ok {
		t.Fatal("app vanished")
	}
	if app.PrivateKey == "local-pem" {
		t.Fatalf("peer edit did not apply: %+v", app)
	}
	if app.ID != "gha_local" {
		t.Fatalf("row id churned: %s", app.ID)
	}
	if apps := st.GithubAppsBySecretID("tc_rid"); len(apps) != 1 {
		t.Fatalf("duplicated rows: %+v", apps)
	}
}

// An app minted here and not yet published — including one still mid-manifest —
// is not in the bundle yet, so a pull must not delete it.
func TestApplyEnvelopeKeepsUnpublishedLocalApp(t *testing.T) {
	st := testStore(t)
	seedAppGroup(t, st)
	if _, err := st.UpsertGithubApp(store.GithubAppRec{
		ID: "gha_pending", SecretID: "tc_rid", Name: "piso-new", ManifestState: "nonce",
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
	if _, ok := st.GithubAppByID("gha_pending"); !ok {
		t.Fatal("in-flight app dropped by a pull")
	}
}

// An app removed on another gateway disappears here too: the bundle is the
// whole group at that version.
func TestApplyEnvelopeDropsAppRemovedElsewhere(t *testing.T) {
	st := testStore(t)
	seedAppGroup(t, st)
	id, err := tc.Generate("https://s", "gw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertGithubApp(store.GithubAppRec{
		ID: "gha_old", SecretID: "tc_rid", AppID: 43, Name: "gone", PrivateKey: "pem",
		Source: store.GithubAppSourceTechnocore, ContentVersion: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(st, id, appEnvelope(t, id, "rid", "piso_github_abc", sharedApp(t), 3)); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GithubAppByAppID(43); ok {
		t.Fatal("app removed elsewhere survived")
	}
}

// A revoked group must drop its apps everywhere, or gateways keep minting
// against a deleted app until the first 401.
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
		t.Fatal("app survived revocation")
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

// A group that arrived from a peer is republished like any other: the envelope
// is authoritative, not the gateway that happens to hold it.
func TestSharePlaintextPublishesReceivedGroup(t *testing.T) {
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
	plain, ok := sharePlaintext(st, sec)
	if !ok {
		t.Fatal("received group not republished")
	}
	b, err := githubapp.DecodeBundle(plain)
	if err != nil || len(b.Apps) != 1 || b.Apps[0].AppID != 42 {
		t.Fatalf("bundle: %+v %v", b, err)
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

// An install flow started here is not part of the bundle, so a sync that lands
// between the click and GitHub's callback must not drop its nonce.
func TestApplyEnvelopeKeepsLocalInstallState(t *testing.T) {
	st := testStore(t)
	seedAppGroup(t, st)
	expires := time.Now().Add(10 * time.Minute)
	if _, err := st.UpsertGithubApp(store.GithubAppRec{
		ID: "gha_local", SecretID: "tc_rid", AppID: 42, PrivateKey: "local-pem",
		InstallState: "nonce-123", StateExpires: expires,
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
	if app.InstallState != "nonce-123" || !app.StateExpires.Equal(expires) {
		t.Fatalf("install flow dropped: %+v", app)
	}
}
