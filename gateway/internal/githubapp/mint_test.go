package githubapp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"piso/gateway/internal/store"
)

func testPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
}

func TestMintCachesToken(t *testing.T) {
	pemBytes := testPEM(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/access_tokens") {
			t.Fatalf("path %s", r.URL.Path)
		}
		io.WriteString(w, `{"token":"ghs_testtoken","expires_at":"2099-01-01T00:00:00Z"}`)
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	c.BaseURL = srv.URL
	c.Now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	app := store.GithubAppRec{
		AppID: 1, PrivateKey: string(pemBytes),
		Installations: []store.GithubInstallation{{ID: 7, Account: "complex-gh"}},
	}
	tok, err := c.Mint(app, "complex-gh")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_testtoken" {
		t.Fatalf("tok %q", tok)
	}
	tok2, err := c.Mint(app, "complex-gh")
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != tok {
		t.Fatalf("cache miss")
	}
	if calls != 1 {
		t.Fatalf("calls %d", calls)
	}
	if len(c.LiveTokens()) != 1 {
		t.Fatalf("live %v", c.LiveTokens())
	}
}

func TestMintRequiresInstallation(t *testing.T) {
	c := NewClient(http.DefaultClient)
	_, err := c.Mint(store.GithubAppRec{AppID: 1, PrivateKey: "x"}, "acme")
	if err == nil {
		t.Fatal("expected error")
	}
}
