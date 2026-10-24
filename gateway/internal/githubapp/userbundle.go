package githubapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// UserBundleVersion guards the dashboard sign-in payload. A gateway refuses a
// version it does not know instead of importing half a session.
const UserBundleVersion = 1

// UserBundle is one person's GitHub OAuth sign-in as it travels between
// enrolled gateways: the token that identifies them, the client app that minted
// it (needed to re-authorize on this gateway), and the identity the token
// resolves to.
//
// It rides in a secret envelope of kind "github-user", sealed per recipient
// gateway like every other secret, so the coordinator only ever holds
// ciphertext. Only a gateway that signed in locally encodes one; a gateway that
// received it materializes the same token as local gateway-only rows and marks
// its GithubUserRec as mirrored, so no peer ever republishes the session.
type UserBundle struct {
	V            int    `json:"v"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
	Token        string `json:"token"`
	Login        string `json:"login"`
	AccountID    int64  `json:"accountId"`
	// Scopes is the space-separated grant GitHub reported at sign-in. Empty
	// means unknown (an older sign-in), not "no scopes".
	Scopes string `json:"scopes,omitempty"`
}

// EncodeUserBundle serializes a sign-in. It is stable: the same session always
// produces the same bytes, so the content hash only changes when the session
// does and reconcile can skip an unchanged republish.
func EncodeUserBundle(b UserBundle) (string, error) {
	if strings.TrimSpace(b.Token) == "" || strings.TrimSpace(b.Login) == "" || b.AccountID <= 0 {
		return "", errors.New("github sign-in is incomplete")
	}
	b.V = UserBundleVersion
	out, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// DecodeUserBundle parses an envelope plaintext strictly. Anything that is not
// a known-version bundle is rejected: a plaintext that happens to sit in the
// same group must never be read as a session.
func DecodeUserBundle(plain string) (UserBundle, error) {
	var b UserBundle
	if strings.TrimSpace(plain) == "" || !strings.HasPrefix(strings.TrimSpace(plain), "{") {
		return b, errors.New("not a github sign-in bundle")
	}
	if err := json.Unmarshal([]byte(plain), &b); err != nil {
		return UserBundle{}, fmt.Errorf("github sign-in bundle: %w", err)
	}
	// A routing-group bundle rides the same envelope channel; it must never be
	// read as a session.
	if strings.Contains(plain, `"apps"`) {
		return UserBundle{}, errors.New("not a github sign-in bundle")
	}
	if b.V != UserBundleVersion {
		return UserBundle{}, fmt.Errorf("unsupported github sign-in bundle version %d", b.V)
	}
	if strings.TrimSpace(b.Token) == "" || strings.TrimSpace(b.Login) == "" || b.AccountID <= 0 {
		return UserBundle{}, errors.New("github sign-in bundle is incomplete")
	}
	return b, nil
}
