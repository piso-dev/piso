package technocore

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"

	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

type remoteGateway struct {
	PrincipalID      string `json:"principal_id"`
	Name             string `json:"name"`
	Status           string `json:"status"`
	EncryptionPubKey string `json:"encryption_pubkey"`
	Fingerprint      string `json:"fingerprint"`
}

// ShareInput is a secret to wrap for enrolled gateways.
type ShareInput struct {
	Name string
	// Kind is the secret's subtype ("telegram", "github-app", ...). The
	// coordinator stores every envelope as a secret resource, so the subtype
	// travels in metadata: without it a peer receives the credential but cannot
	// tell what it is for.
	Kind         string
	Placeholder  string
	EnvKey       string
	AllowedHosts []string
	Plaintext    string
	Recipients   []string // principal ids or fingerprints; empty = all
	GatewayOnly  bool     // never substituted into worker traffic
	// BaseVersion is the coordinator version this write is based on. It is
	// omitted (0) when the writer has never published the secret, so a stale
	// writer can be told to re-sync instead of overwriting newer state.
	BaseVersion int
}

// Share wraps plaintext for account gateways and POSTs ciphertext to the server.
// On success the matching local vault row is stamped with the coordinator
// resource_id, content hash, and recipient set so publishAllSecrets can skip
// an unchanged republish.
func Share(st *store.Store, dataDir string, in ShareInput) (map[string]any, error) {
	id, err := tc.Load(tc.Path(dataDir))
	if err != nil {
		return nil, fmt.Errorf("not enrolled: %w", err)
	}
	if id.Status != tc.StatusActive {
		return nil, fmt.Errorf("gateway is not enrolled")
	}
	gateways, err := fetchGateways(id)
	if err != nil {
		return nil, err
	}
	out, recipients, err := shareWith(id, gateways, in, http.MethodPost, strings.TrimRight(id.ServerURL, "/")+"/api/v1/secrets")
	if err != nil {
		return nil, err
	}
	stampAfterShare(st, in, out, recipients)
	return out, nil
}

func shareWith(id tc.Identity, gateways []remoteGateway, in ShareInput, method, url string) (map[string]any, []string, error) {
	want := map[string]bool{}
	for _, r := range in.Recipients {
		want[strings.ToLower(r)] = true
	}
	dek, nonce, ct, err := tc.SealPayload([]byte(in.Plaintext))
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256([]byte(in.Plaintext))
	wraps := []map[string]string{}
	var recipientIDs []string
	for _, g := range gateways {
		if len(want) > 0 && !want[strings.ToLower(g.PrincipalID)] && !want[strings.ToLower(g.Fingerprint)] {
			continue
		}
		if g.EncryptionPubKey == "" {
			continue
		}
		blob, err := tc.WrapDEK(g.EncryptionPubKey, dek)
		if err != nil {
			return nil, nil, fmt.Errorf("wrap %s: %w", g.Name, err)
		}
		wraps = append(wraps, map[string]string{
			"principal_id":     g.PrincipalID,
			"wrapped_data_key": base64.StdEncoding.EncodeToString(blob),
		})
		recipientIDs = append(recipientIDs, g.PrincipalID)
	}
	if len(wraps) == 0 {
		return nil, nil, fmt.Errorf("no gateway encryption keys to wrap")
	}
	body, _ := json.Marshal(baseShareBody(in, sum[:], nonce, ct, wraps))
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := tc.SignRequest(req, id, body); err != nil {
		return nil, nil, err
	}
	res, err := serverHTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 201 && res.StatusCode != 200 {
		return nil, nil, fmt.Errorf("share HTTP %d: %s", res.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, err
	}
	return out, recipientIDs, nil
}

func fetchGateways(id tc.Identity) ([]remoteGateway, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(id.ServerURL, "/")+"/api/v1/gateways", nil)
	if err != nil {
		return nil, err
	}
	if err := tc.SignRequest(req, id, nil); err != nil {
		return nil, err
	}
	res, err := serverHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("gateways HTTP %d: %s", res.StatusCode, raw)
	}
	var out struct {
		Gateways []remoteGateway `json:"gateways"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Gateways, nil
}

// PublishNow republishes every local secret immediately instead of waiting for
// the reconcile pass. Connecting a bot uses it so the new credential has a
// coordinator resource id before anything tries to use it: the allow-list and
// the claim code live on the coordinator, keyed by that id.
func PublishNow(st *store.Store, dataDir string) error {
	return publishAllSecrets(st, dataDir)
}

// publishAllSecrets reconciles every local secret that has a value with
// Technocore. Synced tc_ rows are included so a newly enrolled gateway can
// still get wraps. Unchanged secrets (same coordinator id, content hash, and
// recipient set) are skipped so reconnects do not mint new rows or bump
// content_version.
func publishAllSecrets(st *store.Store, dataDir string) error {
	id, err := tc.Load(tc.Path(dataDir))
	if err != nil {
		return fmt.Errorf("not enrolled: %w", err)
	}
	if id.Status != tc.StatusActive {
		return fmt.Errorf("gateway is not enrolled")
	}
	gateways, err := fetchGateways(id)
	if err != nil {
		return err
	}
	recipients := wrapRecipientIDs(gateways, nil)
	var first error
	for _, rec := range st.Secrets() {
		if rec.Value == "" {
			continue
		}
		hash := plaintextHash(rec.Value)
		if rec.TechnocoreID != "" && rec.PublishedHash == hash && sameRecipients(rec.PublishedRecipients, recipients) {
			continue
		}
		method := http.MethodPost
		url := strings.TrimRight(id.ServerURL, "/") + "/api/v1/secrets"
		// A stamped row whose plaintext changed must update that resource.
		// POST would 409 on the same placeholder and stall reconcile.
		if rec.TechnocoreID != "" && rec.PublishedHash != "" && rec.PublishedHash != hash {
			method = http.MethodPut
			url += "/" + rec.TechnocoreID
		}
		in := ShareInput{
			Name:         rec.Name,
			Kind:         rec.Kind,
			Placeholder:  rec.Placeholder,
			EnvKey:       rec.EnvKey,
			AllowedHosts: rec.AllowedHosts,
			Plaintext:    rec.Value,
			GatewayOnly:  rec.GatewayOnly,
			BaseVersion:  rec.ContentVersion,
		}
		out, wrapped, err := shareWith(id, gateways, in, method, url)
		if err != nil {
			log.Printf("technocore: publish %s: %v", rec.Placeholder, err)
			if first == nil {
				first = err
			}
			continue
		}
		stampAfterShare(st, in, out, wrapped)
		log.Printf("technocore: published %s as %s", rec.Placeholder, resourceIDOf(out))
	}
	return first
}

// PublishPlaceholder wraps an existing local vault secret for other gateways.
func PublishPlaceholder(st *store.Store, dataDir, placeholder string, recipients []string) (map[string]any, error) {
	rec, ok := st.SecretByPlaceholder(placeholder)
	if !ok {
		return nil, fmt.Errorf("unknown placeholder %s", placeholder)
	}
	if rec.Value == "" {
		return nil, fmt.Errorf("secret has no local value")
	}
	return Share(st, dataDir, ShareInput{
		Name:         rec.Name,
		Kind:         rec.Kind,
		Placeholder:  rec.Placeholder,
		EnvKey:       rec.EnvKey,
		AllowedHosts: rec.AllowedHosts,
		Plaintext:    rec.Value,
		Recipients:   recipients,
		GatewayOnly:  rec.GatewayOnly,
		BaseVersion:  rec.ContentVersion,
	})
}

// Revoke tombstones a secret on the coordinator and returns the tombstone's
// content_version. Deleting the local row alone would leave every peer that
// still holds a copy free to publish it back, so the deletion is published as
// an ordinary version-ordered change instead.
func Revoke(dataDir, resourceID string, baseVersion int) (int, error) {
	if resourceID == "" {
		return 0, fmt.Errorf("no coordinator resource id")
	}
	id, err := tc.Load(tc.Path(dataDir))
	if err != nil {
		return 0, fmt.Errorf("not enrolled: %w", err)
	}
	if id.Status != tc.StatusActive {
		return 0, fmt.Errorf("gateway is not enrolled")
	}
	body, _ := json.Marshal(map[string]any{"base_version": baseVersion})
	url := strings.TrimRight(id.ServerURL, "/") + "/api/v1/secrets/" + resourceID
	req, err := http.NewRequest(http.MethodDelete, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := tc.SignRequest(req, id, body); err != nil {
		return 0, err
	}
	res, err := serverHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 200 {
		return 0, fmt.Errorf("revoke HTTP %d: %s", res.StatusCode, raw)
	}
	var out struct {
		ContentVersion int `json:"content_version"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, err
	}
	return out.ContentVersion, nil
}

func baseShareBody(in ShareInput, hash, nonce, ct []byte, wraps []map[string]string) map[string]any {
	body := map[string]any{
		"name":         in.Name,
		"algorithm":    tc.EnvelopeAlg,
		"nonce":        base64.StdEncoding.EncodeToString(nonce),
		"ciphertext":   base64.StdEncoding.EncodeToString(ct),
		"content_hash": hex.EncodeToString(hash),
		"metadata": map[string]any{
			"placeholder":  in.Placeholder,
			"envKey":       in.EnvKey,
			"allowedHosts": in.AllowedHosts,
			// kind travels with the ciphertext so a receiving gateway knows the
			// secret's subtype. The coordinator only stores secret resources, so
			// this is the only channel that can carry it.
			"kind": in.Kind,
			// gatewayOnly travels with the ciphertext so the receiving
			// gateway also refuses to substitute it into worker traffic.
			"gatewayOnly": in.GatewayOnly,
		},
		"wraps": wraps,
	}
	if in.BaseVersion > 0 {
		body["base_version"] = in.BaseVersion
	}
	return body
}

func plaintextHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sameRecipients(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(as, bs)
}

func wrapRecipientIDs(gateways []remoteGateway, want map[string]bool) []string {
	var ids []string
	for _, g := range gateways {
		if len(want) > 0 && !want[strings.ToLower(g.PrincipalID)] && !want[strings.ToLower(g.Fingerprint)] {
			continue
		}
		if g.EncryptionPubKey == "" {
			continue
		}
		ids = append(ids, g.PrincipalID)
	}
	slices.Sort(ids)
	return ids
}

func resourceIDOf(out map[string]any) string {
	v, _ := out["resource_id"].(string)
	return strings.TrimSpace(v)
}

func stampAfterShare(st *store.Store, in ShareInput, out map[string]any, recipients []string) {
	if st == nil {
		return
	}
	rid := resourceIDOf(out)
	if rid == "" {
		return
	}
	var rec store.SecretRec
	var ok bool
	if in.Placeholder != "" {
		rec, ok = st.SecretByPlaceholder(in.Placeholder)
	}
	if !ok {
		for _, s := range st.Secrets() {
			if s.Value != "" && s.Value == in.Plaintext {
				rec, ok = s, true
				break
			}
		}
	}
	if !ok {
		return
	}
	ids := append([]string(nil), recipients...)
	slices.Sort(ids)
	rec.TechnocoreID = rid
	rec.PublishedHash = plaintextHash(in.Plaintext)
	rec.PublishedRecipients = ids
	rec.Revoked = false
	if v, ok := out["content_version"].(float64); ok {
		rec.ContentVersion = int(v)
	}
	if err := st.ReplaceSecret(rec); err != nil {
		log.Printf("technocore: stamp %s: %v", rec.Placeholder, err)
	}
}
