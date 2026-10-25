package githubapp

import (
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

func TestUserBundleRoundTrip(t *testing.T) {
	in := UserBundle{
		ClientID: "Iv1.abc", ClientSecret: "client-secret-value", Token: "gho_token_value",
		Login: "octocat", AccountID: 583231, Scopes: "read:user read:org",
	}
	plain, err := EncodeUserBundle(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeUserBundle(plain)
	if err != nil {
		t.Fatal(err)
	}
	if out.V != UserBundleVersion || out.Login != in.Login || out.AccountID != in.AccountID ||
		out.Token != in.Token || out.ClientID != in.ClientID || out.ClientSecret != in.ClientSecret ||
		out.Scopes != in.Scopes {
		t.Fatalf("round trip: %+v", out)
	}
	// Stable bytes: the content hash must change only when the session does, or
	// every reconcile would republish it.
	again, err := EncodeUserBundle(in)
	if err != nil || again != plain {
		t.Fatalf("encoding not stable: %v", err)
	}
}

func TestUserBundleRejectsIncomplete(t *testing.T) {
	for name, in := range map[string]UserBundle{
		"no token":     {Login: "octocat", AccountID: 1},
		"no login":     {Token: "gho_x", AccountID: 1},
		"no account":   {Token: "gho_x", Login: "octocat"},
		"blank spaces": {Token: "  ", Login: "  ", AccountID: 1},
	} {
		if _, err := EncodeUserBundle(in); err == nil {
			t.Fatalf("%s: encoded an incomplete session", name)
		}
	}
}

func TestDecodeUserBundleRejectsNonBundles(t *testing.T) {
	for name, plain := range map[string]string{
		"empty":        "",
		"plain token":  "gho_token_value",
		"unknown":      `{"v":99,"token":"gho_x","login":"octocat","accountId":1}`,
		"array":        `[]`,
		"no version":   `{"token":"gho_x","login":"octocat","accountId":1}`,
		"incomplete":   `{"v":1,"token":"gho_x"}`,
		"app bundle":   `{"v":1,"apps":[]}`,
		"malformed":    `{"v":1,`,
		"blank token":  `{"v":1,"token":" ","login":"octocat","accountId":1}`,
		"zero account": `{"v":1,"token":"gho_x","login":"octocat"}`,
	} {
		if _, err := DecodeUserBundle(plain); err == nil {
			t.Fatalf("%s: accepted %q as a sign-in", name, plain)
		}
	}
}

// The two bundle kinds share an envelope channel, so neither may be read as the
// other: a GitHub App bundle must not decode as a session.
func TestUserAndAppBundlesAreDistinct(t *testing.T) {
	appBundle, err := EncodeBundle([]store.GithubAppRec{{ID: "a", AppID: 1, PrivateKey: "pem"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeUserBundle(appBundle); err == nil {
		t.Fatal("app bundle decoded as a sign-in")
	}
	userBundle, err := EncodeUserBundle(UserBundle{Token: "gho_x", Login: "octocat", AccountID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBundle(userBundle); err == nil {
		t.Fatal("sign-in decoded as an app bundle")
	}
	if !strings.Contains(userBundle, `"token":"gho_x"`) {
		t.Fatalf("unexpected shape: %s", userBundle)
	}
}
