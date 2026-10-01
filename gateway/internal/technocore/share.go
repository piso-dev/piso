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
	Name         string
	Placeholder  string
	EnvKey       string
	AllowedHosts []string
	Plaintext    string
	Recipients   []string // principal ids or fingerprints; empty = all
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
	body, _ := json.Marshal(map[string]any{
		"name":         in.Name,
		"algorithm":    tc.EnvelopeAlg,
		"nonce":        base64.StdEncoding.EncodeToString(nonce),
		"ciphertext":   base64.StdEncoding.EncodeToString(ct),
		"content_hash": hex.EncodeToString(sum[:]),
		"metadata": map[string]any{
			"placeholder":  in.Placeholder,
			"envKey":       in.EnvKey,
			"allowedHosts": in.AllowedHosts,
		},
		"wraps": wraps,
	})
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
		out, wrapped, err := shareWith(id, gateways, ShareInput{
			Name:         rec.Name,
			Placeholder:  rec.Placeholder,
			EnvKey:       rec.EnvKey,
			AllowedHosts: rec.AllowedHosts,
			Plaintext:    rec.Value,
		}, method, url)
		if err != nil {
			log.Printf("technocore: publish %s: %v", rec.Placeholder, err)
			if first == nil {
				first = err
			}
			continue
		}
		stampAfterShare(st, ShareInput{Placeholder: rec.Placeholder, Plaintext: rec.Value}, out, wrapped)
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
		Placeholder:  rec.Placeholder,
		EnvKey:       rec.EnvKey,
		AllowedHosts: rec.AllowedHosts,
		Plaintext:    rec.Value,
		Recipients:   recipients,
	})
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
	rec, ok := st.SecretByPlaceholder(in.Placeholder)
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
	if err := st.ReplaceSecret(rec); err != nil {
		log.Printf("technocore: stamp %s: %v", rec.Placeholder, err)
	}
}
