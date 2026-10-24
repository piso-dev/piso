package githubapp

import (
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

func bundleApp(appID int64, account string) store.GithubAppRec {
	return store.GithubAppRec{
		ID: "gha_1", SecretID: "sec_1", AppID: appID, Name: "piso-test", Slug: "piso-test",
		Owner: "octocat", OwnerType: "User", PrivateKey: "-----BEGIN RSA PRIVATE KEY-----\nkey\n-----END RSA PRIVATE KEY-----",
		Installations: []store.GithubInstallation{{ID: appID + 100, Account: account, AccountType: "User", Repos: []string{account + "/repo"}}},
	}
}

func TestEncodeDecodeBundleRoundTrip(t *testing.T) {
	plain, err := EncodeBundle([]store.GithubAppRec{bundleApp(7, "octocat")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeBundle(plain)
	if err != nil {
		t.Fatal(err)
	}
	if b.V != BundleVersion || len(b.Apps) != 1 {
		t.Fatalf("bundle: %+v", b)
	}
	if b.Apps[0].AppID != 7 || b.Apps[0].PrivateKey == "" {
		t.Fatalf("app: %+v", b.Apps[0])
	}
	if len(b.Apps[0].Installations) != 1 || b.Apps[0].Installations[0].Repos[0] != "octocat/repo" {
		t.Fatalf("installations: %+v", b.Apps[0].Installations)
	}
}

// An unregistered app holds no credential, so a group of only unregistered
// apps has nothing to publish until the manifest flow finishes.
func TestEncodeBundleSkipsUnregisteredApps(t *testing.T) {
	unregistered := bundleApp(0, "octocat")
	unregistered.AppID, unregistered.PrivateKey = 0, ""
	if _, err := EncodeBundle([]store.GithubAppRec{unregistered}); err == nil {
		t.Fatal("expected an error for a group with no registered app")
	}
}

func TestEncodeBundleIsOrderIndependent(t *testing.T) {
	a, b := bundleApp(7, "octocat"), bundleApp(8, "acme")
	b.ID = "gha_2"
	first, err := EncodeBundle([]store.GithubAppRec{a, b})
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeBundle([]store.GithubAppRec{b, a})
	if err != nil {
		t.Fatal(err)
	}
	// Stable ordering keeps the content hash stable, so reconcile can skip an
	// unchanged group instead of bumping content_version every pass.
	if first != second {
		t.Fatalf("unstable encoding:\n%s\n%s", first, second)
	}
}

// An empty bundle is valid: it means the origin group holds no app a peer
// could use, and it must clear any mirrors rather than be rejected.
func TestDecodeBundleAcceptsEmptyAppList(t *testing.T) {
	b, err := DecodeBundle(`{"v":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Apps) != 0 {
		t.Fatalf("apps: %+v", b.Apps)
	}
}

func TestDecodeBundleRejectsNonBundle(t *testing.T) {
	for _, plain := range []string{"", "ghp_plaintext_pat", `{"v":99,"apps":[]}`, `[]`} {
		if _, err := DecodeBundle(plain); err == nil {
			t.Fatalf("accepted %q", plain)
		}
	}
}

func TestBundleRecsMarkMirrors(t *testing.T) {
	plain, err := EncodeBundle([]store.GithubAppRec{bundleApp(7, "octocat")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeBundle(plain)
	if err != nil {
		t.Fatal(err)
	}
	recs := b.Recs("tc_rid", "rid", 4)
	if len(recs) != 1 {
		t.Fatalf("recs: %+v", recs)
	}
	got := recs[0]
	if got.Source != store.GithubAppSourceTechnocore || !got.Mirrored() {
		t.Fatalf("not a mirror: %+v", got)
	}
	if got.ID != MirroredAppID(7) || got.SecretID != "tc_rid" || got.ContentVersion != 4 {
		t.Fatalf("identity: %+v", got)
	}
	if got.PrivateKey == "" || strings.HasPrefix(got.PrivateKey, "piso_") {
		t.Fatalf("pem: %q", got.PrivateKey)
	}
}
