package technocore

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

type envelope struct {
	ResourceID     string         `json:"resource_id"`
	Name           string         `json:"name"`
	Kind           string         `json:"kind"`
	Metadata       map[string]any `json:"metadata"`
	Algorithm      string         `json:"algorithm"`
	ContentVersion int            `json:"content_version"`
	ContentHash    string         `json:"content_hash"`
	Ciphertext     string         `json:"ciphertext"`
	Nonce          string         `json:"nonce"`
	WrappedDataKey string         `json:"wrapped_data_key"`
}

// Run watches identity.json. While unpaired it polls pairing status.
// Once enrolled it holds a WebSocket and refreshes envelopes on ready /
// envelopes_changed.
func Run(st *store.Store, dataDir string) {
	path := tc.Path(dataDir)
	backoff := time.Second
	for {
		id, err := tc.Load(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("technocore: %v", err)
			}
			time.Sleep(5 * time.Second)
			continue
		}
		if id.Status == tc.StatusPending && id.PairingID != "" {
			if err := tickPending(path, id); err != nil {
				log.Printf("technocore: %v", err)
			}
			time.Sleep(2 * time.Second)
			continue
		}
		if id.Status != tc.StatusActive {
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("technocore: connecting %s", id.ServerURL)
		ready, err := connectSession(st, id)
		if err != nil {
			log.Printf("technocore: websocket: %v", err)
			setLive(func(s *Status) { s.LastError = err.Error() })
		}
		if ready {
			backoff = time.Second
		} else {
			time.Sleep(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		time.Sleep(backoff)
	}
}

func tickPending(path string, id tc.Identity) error {
	status, principal, err := poll(id)
	if err != nil {
		return err
	}
	if status == "approved" && principal != "" {
		id.Status = tc.StatusActive
		id.PrincipalID = principal
		if err := tc.Save(path, id); err != nil {
			return err
		}
		log.Printf("technocore: pairing approved (%s)", principal)
	}
	return nil
}

func poll(id tc.Identity) (string, string, error) {
	u := fmt.Sprintf("%s/api/v1/pairing-requests/%s", strings.TrimRight(id.ServerURL, "/"), id.PairingID)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set(tc.HeaderPollToken, id.PollToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 200 {
		return "", "", fmt.Errorf("poll HTTP %d", res.StatusCode)
	}
	var out struct {
		Status      string `json:"status"`
		PrincipalID string `json:"principal_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", err
	}
	return out.Status, out.PrincipalID, nil
}

func syncSecrets(st *store.Store, id tc.Identity) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(id.ServerURL, "/")+"/api/v1/sync/secrets", nil)
	if err != nil {
		return err
	}
	if err := tc.SignRequest(req, id, nil); err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode != 200 {
		return fmt.Errorf("sync HTTP %d: %s", res.StatusCode, raw)
	}
	var out struct {
		Objects []envelope `json:"objects"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	seen := map[string]bool{}
	applied := 0
	skipped := 0
	for _, obj := range out.Objects {
		secretID := "tc_" + obj.ResourceID
		if err := applyEnvelope(st, id, obj); err != nil {
			log.Printf("technocore: skip %s: %v", obj.ResourceID, err)
			skipped++
			continue
		}
		seen[secretID] = true
		applied++
	}
	removed := 0
	for _, rec := range st.Secrets() {
		if !strings.HasPrefix(rec.ID, "tc_") {
			continue
		}
		if seen[rec.ID] {
			continue
		}
		if err := st.DeleteSecret(rec.ID); err != nil {
			log.Printf("technocore: delete %s: %v", rec.ID, err)
			continue
		}
		removed++
	}
	setLive(func(s *Status) {
		s.LastSync = time.Now().UTC()
		s.LastError = ""
		s.EnvelopeSkip = skipped
	})
	if applied > 0 || removed > 0 {
		log.Printf("technocore: applied %d envelopes, removed %d", applied, removed)
	}
	return nil
}

func applyEnvelope(st *store.Store, id tc.Identity, obj envelope) error {
	if obj.Algorithm != "" && obj.Algorithm != tc.EnvelopeAlg {
		return fmt.Errorf("unsupported algorithm %q", obj.Algorithm)
	}
	ct, err := base64.StdEncoding.DecodeString(obj.Ciphertext)
	if err != nil {
		return err
	}
	nonce, err := base64.StdEncoding.DecodeString(obj.Nonce)
	if err != nil {
		return err
	}
	wrap, err := base64.StdEncoding.DecodeString(obj.WrappedDataKey)
	if err != nil {
		return err
	}
	plaintext, err := tc.OpenEnvelope(id, wrap, nonce, ct)
	if err != nil {
		return err
	}
	placeholder, _ := obj.Metadata["placeholder"].(string)
	if placeholder == "" {
		idhex := strings.ReplaceAll(obj.ResourceID, "-", "")
		if len(idhex) > 16 {
			idhex = idhex[:16]
		}
		placeholder = "piso_tc_" + idhex
	}
	envKey, _ := obj.Metadata["envKey"].(string)
	hosts := stringSlice(obj.Metadata["allowedHosts"])
	secretID := "tc_" + obj.ResourceID
	rec := store.SecretRec{
		ID:           secretID,
		Name:         obj.Name,
		Placeholder:  placeholder,
		Value:        string(plaintext),
		EnvKey:       envKey,
		AllowedHosts: hosts,
		CreatedAt:    time.Now().UTC(),
	}
	if existing, ok := st.SecretByID(secretID); ok {
		rec.CreatedAt = existing.CreatedAt
		if err := st.ReplaceSecret(rec); err != nil {
			return err
		}
	} else {
		if err := st.AddSecret(rec); err != nil {
			return err
		}
	}
	return st.SyncRulesForSecret(rec)
}

func stringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, _ := item.(string)
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
