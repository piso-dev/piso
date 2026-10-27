package githubapp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"piso/gateway/internal/store"
)

// BundleVersion is the payload version carried inside a shared GitHub App
// routing-group secret. A peer refuses a bundle it does not understand rather
// than importing half of it.
const BundleVersion = 1

// Bundle is the plaintext of a github-app secret. The vault row itself has no
// value: the credential *is* the app record, so sharing the group means
// shipping the record. Technocore seals this whole struct as the envelope
// ciphertext, so the PEM travels only between enrolled gateways.
type Bundle struct {
	V    int         `json:"v"`
	Apps []BundleApp `json:"apps"`
}

// BundleApp is one GitHub App inside a routing group. Installations travel
// with it because routing needs them before the first API call, and a peer
// holding a valid PEM can refresh them at any time.
type BundleApp struct {
	AppID         int64                `json:"appId"`
	Name          string               `json:"name,omitempty"`
	Slug          string               `json:"slug,omitempty"`
	OwnerID       int64                `json:"ownerId,omitempty"`
	Owner         string               `json:"owner,omitempty"`
	OwnerType     string               `json:"ownerType,omitempty"`
	PrivateKey    string               `json:"privateKey"`
	ClientID      string               `json:"clientId,omitempty"`
	ClientSecret  string               `json:"clientSecret,omitempty"`
	WebhookSecret string               `json:"webhookSecret,omitempty"`
	Installations []BundleInstallation `json:"installations,omitempty"`
}

// BundleInstallation is one account/org the app is installed on, plus the
// repository snapshot the origin gateway last fetched.
type BundleInstallation struct {
	ID          int64             `json:"id"`
	Account     string            `json:"account"`
	AccountType string            `json:"accountType,omitempty"`
	HTMLURL     string            `json:"htmlUrl,omitempty"`
	Repos       []string          `json:"repos,omitempty"`
	Permissions map[string]string `json:"permissions,omitempty"`
}

// EncodeBundle renders apps as the plaintext of a shared routing-group secret.
// Apps are ordered by AppID so an unchanged group hashes to an unchanged
// content hash and the reconcile pass can skip it.
func EncodeBundle(apps []store.GithubAppRec) (string, error) {
	if len(apps) == 0 {
		return "", fmt.Errorf("github app: nothing to share")
	}
	ordered := append([]store.GithubAppRec(nil), apps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].AppID < ordered[j].AppID })
	b := Bundle{V: BundleVersion, Apps: make([]BundleApp, 0, len(ordered))}
	for _, app := range ordered {
		if app.AppID == 0 || strings.TrimSpace(app.PrivateKey) == "" {
			// An unregistered app holds no credential; there is nothing a peer
			// could mint with, so it stays local until it is registered.
			continue
		}
		entry := BundleApp{
			AppID: app.AppID, Name: app.Name, Slug: app.Slug,
			OwnerID: app.OwnerID, Owner: app.Owner, OwnerType: app.OwnerType,
			PrivateKey: app.PrivateKey, ClientID: app.ClientID,
			ClientSecret: app.ClientSecret, WebhookSecret: app.WebhookSecret,
		}
		for _, inst := range app.Installations {
			if inst.ID == 0 || inst.Account == "" {
				continue
			}
			entry.Installations = append(entry.Installations, BundleInstallation{
				ID: inst.ID, Account: inst.Account, AccountType: inst.AccountType,
				HTMLURL: inst.HTMLURL, Repos: append([]string(nil), inst.Repos...), Permissions: inst.Permissions,
			})
		}
		b.Apps = append(b.Apps, entry)
	}
	if len(b.Apps) == 0 {
		return "", fmt.Errorf("github app: no registered apps to share")
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// DecodeBundle parses the plaintext of a github-app secret. It is strict on
// purpose: a plaintext that is not a bundle (a PAT written into a group by an
// older gateway, say) must fail instead of being imported as zero apps.
func DecodeBundle(plain string) (Bundle, error) {
	var b Bundle
	trimmed := strings.TrimSpace(plain)
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return Bundle{}, fmt.Errorf("github app: not a bundle")
	}
	if err := json.Unmarshal([]byte(trimmed), &b); err != nil {
		return Bundle{}, fmt.Errorf("github app: bad bundle: %w", err)
	}
	// A github-user bundle (the dashboard sign-in) rides the same envelope
	// channel. Reading one as a group with zero apps would prune every mirror,
	// so the two shapes are kept apart on their discriminating fields.
	var probe struct {
		Token string `json:"token"`
		Login string `json:"login"`
	}
	if json.Unmarshal([]byte(trimmed), &probe) == nil && (probe.Token != "" || probe.Login != "") {
		return Bundle{}, fmt.Errorf("github app: not a bundle")
	}
	if b.V == 0 || b.V > BundleVersion {
		return Bundle{}, fmt.Errorf("github app: unsupported bundle version %d", b.V)
	}
	valid := b.Apps[:0]
	for _, app := range b.Apps {
		if app.AppID == 0 || strings.TrimSpace(app.PrivateKey) == "" {
			continue
		}
		valid = append(valid, app)
	}
	b.Apps = valid
	return b, nil
}

// Recs converts a bundle into gateway records bound to secretID. Source records
// that the app was last written on another gateway; it is provenance, not a
// permission — any gateway may edit any app in the group.
func (b Bundle) Recs(secretID, resourceID string, version int) []store.GithubAppRec {
	out := make([]store.GithubAppRec, 0, len(b.Apps))
	for _, app := range b.Apps {
		rec := store.GithubAppRec{
			ID:             MirroredAppID(app.AppID),
			SecretID:       secretID,
			Source:         store.GithubAppSourceTechnocore,
			TechnocoreID:   resourceID,
			ContentVersion: version,
			AppID:          app.AppID, Name: app.Name, Slug: app.Slug,
			OwnerID: app.OwnerID, Owner: app.Owner, OwnerType: app.OwnerType,
			PrivateKey: app.PrivateKey, ClientID: app.ClientID,
			ClientSecret: app.ClientSecret, WebhookSecret: app.WebhookSecret,
		}
		for _, inst := range app.Installations {
			rec.Installations = append(rec.Installations, store.GithubInstallation{
				ID: inst.ID, Account: inst.Account, AccountType: inst.AccountType,
				HTMLURL: inst.HTMLURL, Repos: append([]string(nil), inst.Repos...), Permissions: inst.Permissions,
			})
		}
		out = append(out, rec)
	}
	return out
}

// MirroredAppID is the deterministic local id for an app this gateway has only
// ever seen in a bundle. It is derived from the GitHub App id so repeated syncs
// land on one row instead of piling up duplicates; a gateway that configured the
// app itself keeps its own id and the sync updates that row in place.
func MirroredAppID(appID int64) string {
	return fmt.Sprintf("gha_tc_%d", appID)
}
