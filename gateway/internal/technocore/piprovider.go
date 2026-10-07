package technocore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
	"piso/internal/piprofile"
	tc "piso/internal/technocore"
)

const piProviderName = "pi-provider"

type piProviderDoc struct {
	ResourceID string `json:"resource_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Metadata   struct {
		SecretID     string  `json:"secretId"`
		BaseURL      string  `json:"baseUrl"`
		DefaultModel *string `json:"defaultModel"`
	} `json:"metadata"`
	UpdatedAt string `json:"updated_at"`
}

// syncPiProvider pulls the account's shared pi provider pointer and writes
// this gateway's models.json. A missing remote record leaves the local
// pointer alone so an unpaired or older coordinator does not wipe it.
func syncPiProvider(st *store.Store, dataDir string, id tc.Identity) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(id.ServerURL, "/")+"/api/v1/pi-provider", nil)
	if err != nil {
		return err
	}
	if err := tc.SignRequest(req, id, nil); err != nil {
		return err
	}
	res, err := serverHTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusNotFound {
		return nil
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("pi-provider HTTP %d: %s", res.StatusCode, raw)
	}
	var doc piProviderDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if doc.Metadata.SecretID == "" && doc.Metadata.BaseURL == "" {
		return nil
	}
	local := st.PiProvider()
	remoteAt := parseTime(doc.UpdatedAt)
	if local != nil && !remoteAt.IsZero() && !local.UpdatedAt.IsZero() && local.UpdatedAt.After(remoteAt) {
		return publishPiProvider(st, id)
	}
	// A coordinator that predates defaultModel omits it. Absence is not an
	// explicit clear, and must not erase a locally saved choice on every sync.
	defaultModel := ""
	if doc.Metadata.DefaultModel != nil {
		defaultModel = *doc.Metadata.DefaultModel
	} else if local != nil && local.SecretID == doc.Metadata.SecretID && local.BaseURL == doc.Metadata.BaseURL {
		defaultModel = local.DefaultModel
	}
	if err := st.SetPiProvider(&store.PiProviderRec{
		SecretID:     doc.Metadata.SecretID,
		BaseURL:      doc.Metadata.BaseURL,
		DefaultModel: defaultModel,
		UpdatedAt:    remoteAt,
	}); err != nil {
		return err
	}
	applyProviderPointer(st, dataDir)
	return nil
}

// PublishPiProvider pushes the local pointer when this gateway is enrolled.
// Not enrolled is not an error: the dashboard save is still local, and the
// next reconcile publishes once pairing completes.
func PublishPiProvider(st *store.Store, dataDir string) error {
	id, err := tc.Load(tc.Path(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if id.Status != tc.StatusActive || id.ServerURL == "" {
		return nil
	}
	return publishPiProvider(st, id)
}

// publishPiProvider pushes the local pointer. The URL is not a secret; the
// secret id is a coordinator resource UUID, not a placeholder or value.
func publishPiProvider(st *store.Store, id tc.Identity) error {
	p := st.PiProvider()
	if p == nil || p.SecretID == "" || p.BaseURL == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]any{
		"name": piProviderName,
		"metadata": map[string]string{
			"secretId":     p.SecretID,
			"baseUrl":      p.BaseURL,
			"defaultModel": p.DefaultModel,
		},
	})
	req, err := http.NewRequest(http.MethodPut, strings.TrimRight(id.ServerURL, "/")+"/api/v1/pi-provider", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := tc.SignRequest(req, id, body); err != nil {
		return err
	}
	res, err := serverHTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		return fmt.Errorf("pi-provider publish HTTP %d: %s", res.StatusCode, raw)
	}
	return nil
}

// applyProviderPointer writes models.json when the pointed-at secret is
// already in this gateway's vault. Called after secret sync and after the
// pointer itself arrives.
func applyProviderPointer(st *store.Store, dataDir string) {
	p := st.PiProvider()
	if p == nil || p.SecretID == "" || p.BaseURL == "" {
		log.Printf("technocore: pi profile: no provider pointer yet")
		return
	}
	rec, ok := secretForPointer(st, p.SecretID)
	if !ok || !strings.HasPrefix(rec.Placeholder, model.PlaceholderPrefix) {
		log.Printf("technocore: pi profile: secret %s not in this vault yet", p.SecretID)
		return
	}
	dir := piProfileDir(dataDir)
	if err := piprofile.WriteProviderModels(dir, rec.Placeholder, p.BaseURL); err != nil {
		log.Printf("technocore: pi profile: write %s: %v", dir, err)
		return
	}
	if err := piprofile.WriteDefaultModel(dir, p.DefaultModel); err != nil {
		log.Printf("technocore: pi profile: default model: %v", err)
		return
	}
	log.Printf("technocore: pi profile: wrote %s (placeholder %s)", filepath.Join(dir, "models.json"), rec.Placeholder)
}

func secretForPointer(st *store.Store, id string) (store.SecretRec, bool) {
	if rec, ok := st.SecretByTechnocoreID(id); ok {
		return rec, true
	}
	if rec, ok := st.SecretByID(id); ok {
		return rec, true
	}
	if rec, ok := st.SecretByID("tc_" + id); ok {
		return rec, true
	}
	return store.SecretRec{}, false
}

// piProfileDir is the host profile workers mount. The gateway container
// bind-mounts ${PISO_DATA} at /data and does not set PISO_DATA, so the
// state-file directory (dataDir) is the one that is actually writable.
// /opt/piso/pi-profile is the worker mount, not a gateway path.
func piProfileDir(dataDir string) string {
	if v := strings.TrimSpace(os.Getenv("PISO_PI_PROFILE")); v != "" {
		return v
	}
	if dataDir != "" {
		return filepath.Join(dataDir, "pi-profile")
	}
	if v := strings.TrimSpace(os.Getenv("PISO_DATA")); v != "" {
		return filepath.Join(v, "pi-profile")
	}
	return "/opt/piso/pi-profile"
}

func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t
}
