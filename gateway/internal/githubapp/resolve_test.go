package githubapp

import "testing"

func TestRequestRoutes(t *testing.T) {
	for _, tc := range []struct{ host, path, body, want string }{
		{"github.com", "/Acme/repo.git/info/refs?service=git-upload-pack", "", "acme"},
		{"api.github.com", "/repos/Acme/repo/issues", "", "acme"},
		{"uploads.github.com", "/repos/Acme/repo/releases/1/assets", "", "acme"},
		{"codeload.github.com", "/Acme/repo/zip/main", "", "acme"},
		{"api.github.com", "/graphql", `{"variables":{"owner":"Acme"}}`, "acme"},
		{"api.github.com", "/graphql", `{"variables":{"owner":"Acme","org":"Other"}}`, "!ambiguous"},
		{"api.github.com", "/user", `{"owner":"Other"}`, ""},
	} {
		if got := Owner(tc.host, tc.path, tc.body); got != tc.want {
			t.Errorf("%s%s: got %q want %q", tc.host, tc.path, got, tc.want)
		}
	}
	if _, err := ResolveAccount([]string{"acme"}, Owner("api.github.com", "/graphql", `{"variables":{"a":{"owner":"acme"},"b":{"owner":"other"}}}`)); err == nil {
		t.Fatal("ambiguous body fell back to sole installation")
	}
	for _, tc := range []struct {
		accounts []string
		owner    string
		ok       bool
	}{
		{[]string{"acme"}, "", true},
		{[]string{"acme", "other"}, "", false},
		{[]string{"acme", "other"}, "ACME", true},
		{[]string{"acme", "ACME"}, "acme", false},
		{[]string{"acme"}, "unknown", false},
	} {
		_, err := ResolveAccount(tc.accounts, tc.owner)
		if (err == nil) != tc.ok {
			t.Errorf("accounts %v owner %q: %v", tc.accounts, tc.owner, err)
		}
	}
}
