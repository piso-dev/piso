package githubapp

import "testing"

func TestOwnerGitHubDotCom(t *testing.T) {
	if g := Owner("github.com", "/complex-gh/piso.git/info/refs", ""); g != "complex-gh" {
		t.Fatalf("got %q", g)
	}
	if g := Owner("github.com", "/settings/apps", ""); g != "" {
		t.Fatalf("reserved path got %q", g)
	}
}

func TestOwnerAPI(t *testing.T) {
	if g := Owner("api.github.com", "/repos/complex-gh/piso/pulls", ""); g != "complex-gh" {
		t.Fatalf("got %q", g)
	}
	if g := Owner("api.github.com", "/orgs/complex-gh/repos", ""); g != "complex-gh" {
		t.Fatalf("got %q", g)
	}
	if g := Owner("api.github.com", "/user", ""); g != "" {
		t.Fatalf("got %q", g)
	}
}

func TestOwnerJSON(t *testing.T) {
	body := `{"repository":{"owner":{"login":"complex-gh"},"name":"piso"}}`
	if g := Owner("api.github.com", "/graphql", body); g != "complex-gh" {
		t.Fatalf("got %q", g)
	}
}
