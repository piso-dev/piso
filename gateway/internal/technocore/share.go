package technocore

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	want := map[string]bool{}
	for _, r := range in.Recipients {
		want[strings.ToLower(r)] = true
	}
	dek, nonce, ct, err := tc.SealPayload([]byte(in.Plaintext))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(in.Plaintext))
	wraps := []map[string]string{}
	for _, g := range gateways {
		if len(want) > 0 && !want[strings.ToLower(g.PrincipalID)] && !want[strings.ToLower(g.Fingerprint)] {
			continue
		}
		if g.EncryptionPubKey == "" {
			continue
		}
		blob, err := tc.WrapDEK(g.EncryptionPubKey, dek)
		if err != nil {
			return nil, fmt.Errorf("wrap %s: %w", g.Name, err)
		}
		wraps = append(wraps, map[string]string{
			"principal_id":      g.PrincipalID,
			"wrapped_data_key":  base64.StdEncoding.EncodeToString(blob),
		})
	}
	if len(wraps) == 0 {
		return nil, fmt.Errorf("no gateway encryption keys to wrap")
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
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(id.ServerURL, "/")+"/api/v1/secrets", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := tc.SignRequest(req, id, body); err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 201 && res.StatusCode != 200 {
		return nil, fmt.Errorf("share HTTP %d: %s", res.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	_ = st
	return out, nil
}

func fetchGateways(id tc.Identity) ([]remoteGateway, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(id.ServerURL, "/")+"/api/v1/gateways", nil)
	if err != nil {
		return nil, err
	}
	if err := tc.SignRequest(req, id, nil); err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
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
