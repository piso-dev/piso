package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"piso/gateway/internal/store"
)

// testGithubAppPEM returns a record with a real RSA key so JWT signing
// succeeds against stubbed GitHub endpoints.
func testGithubAppPEM(t *testing.T) store.GithubAppRec {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemTxt := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return store.GithubAppRec{
		AppID: 42, Name: "existing-app", Slug: "existing-app",
		PrivateKey: pemTxt,
	}
}
