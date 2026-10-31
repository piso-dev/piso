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
		ActiveID     string  `json:"activeId"`
		Providers    []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			SecretID     string `json:"secretId"`
			BaseURL      string `json:"baseUrl"`
			DefaultModel string `json:"defaultModel"`
			// The coordinator mirrors the profile's policy fields. They are
			// pointers because absence and an empty value mean different
			// things: a coordinator that predates the field omits it (keep the
			// local value), while an explicit "" or [] is a decision the
			// operator made on a gateway and must travel.
			Kind           *string   `json:"kind"`
			APIType        *string   `json:"api"`
			ApprovedModels *[]string `json:"approvedModels"`
		} `json:"providers"`
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
	if len(doc.Metadata.Providers) == 0 && doc.Metadata.SecretID == "" && doc.Metadata.BaseURL == "" {
		return nil
	}
	local := st.PiProvider()
	remoteAt := parseTime(doc.UpdatedAt)
	if local != nil && !remoteAt.IsZero() && !local.UpdatedAt.IsZero() && local.UpdatedAt.After(remoteAt) {
		return publishPiProvider(st, id)
	}
	// A coordinator that knows the whole stack sends it. Merge each remote
	// profile into the local stack, keeping any local-only profile, then
	// select the active one. A coordinator that predates the stack sends the
	// single active pointer and lands in the legacy branch below.
	if len(doc.Metadata.Providers) > 0 {
		for _, rp := range doc.Metadata.Providers {
			if strings.TrimSpace(rp.ID) == "" || rp.SecretID == "" || rp.BaseURL == "" {
				continue
			}
			rec := mergePiProvider(st.PiProviderByID(rp.ID), store.PiProviderRec{
				ID:           rp.ID,
				Name:         rp.Name,
				SecretID:     rp.SecretID,
				BaseURL:      rp.BaseURL,
				DefaultModel: rp.DefaultModel,
				UpdatedAt:    remoteAt,
			}, rp.Kind, rp.APIType, rp.ApprovedModels)
			if err := st.UpsertPiProvider(rec); err != nil {
				return err
			}
		}
		activeID := strings.TrimSpace(doc.Metadata.ActiveID)
		if activeID == "" {
			// Fall back to matching the legacy active fields.
			for _, rp := range doc.Metadata.Providers {
				if rp.SecretID == doc.Metadata.SecretID && rp.BaseURL == doc.Metadata.BaseURL {
					activeID = rp.ID
					break
				}
			}
		}
		if activeID != "" {
			if err := st.ActivatePiProvider(activeID); err != nil {
				return err
			}
		}
		applyProviderPointer(st, dataDir)
		return nil
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

// publishPiProvider pushes the whole dashboard stack. The URL is not a
// secret; a secret id is a coordinator resource UUID, not a placeholder or
// value. The legacy secretId/baseUrl/defaultModel fields stay populated from
// the active profile so an older coordinator still works.
func publishPiProvider(st *store.Store, id tc.Identity) error {
	profiles := st.PiProviderStack()
	// The published profile is the account-wide shape of one stack entry. The
	// approved list travels with it, and is always present (possibly empty) so a
	// list cleared on this gateway clears on the others; a peer that predates the
	// field simply ignores it. The catalog does not travel: it is fetched by each
	// gateway and is not account configuration.
	type publishedProvider struct {
		ID             string   `json:"id"`
		Name           string   `json:"name"`
		SecretID       string   `json:"secretId"`
		BaseURL        string   `json:"baseUrl"`
		Kind           string   `json:"kind"`
		APIType        string   `json:"api"`
		DefaultModel   string   `json:"defaultModel"`
		ApprovedModels []string `json:"approvedModels"`
	}
	providers := make([]publishedProvider, 0, len(profiles))
	for _, p := range profiles {
		if p.SecretID == "" || p.BaseURL == "" {
			continue
		}
		// A nil slice marshals as null, which a reader takes for an omitted
		// field. Normalize to an empty list so "only the default" is a value.
		approved := cleanedModelIDs(p.ApprovedModels)
		providers = append(providers, publishedProvider{
			ID:             p.ID,
			Name:           p.Name,
			SecretID:       p.SecretID,
			BaseURL:        p.BaseURL,
			Kind:           p.Kind,
			APIType:        p.APIType,
			DefaultModel:   p.DefaultModel,
			ApprovedModels: approved,
		})
	}
	if len(providers) == 0 {
		return nil
	}
	meta := map[string]any{"providers": providers}
	if active := st.PiProvider(); active != nil {
		meta["secretId"] = active.SecretID
		meta["baseUrl"] = active.BaseURL
		meta["defaultModel"] = active.DefaultModel
		meta["activeId"] = active.ID
	}
	body, _ := json.Marshal(map[string]any{
		"name":     piProviderName,
		"metadata": meta,
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

// mergePiProvider folds a coordinator's copy of one profile into the local
// record. The coordinator mirrors the account-wide fields — endpoint, secret,
// default model — plus, when it understands them, the kind, the pi api adapter,
// and the approved-model list.
//
// Two rules keep a round trip from destroying local state. Fields the payload
// carries are authoritative, so a list cleared on one gateway reaches the
// others. Fields it does not carry are left as this gateway has them: the model
// catalog is fetched here (never published), and a coordinator that predates a
// field omits it, which must not read as "clear it" — that is what erased an
// approved list on every reconcile.
func mergePiProvider(local *store.PiProviderRec, remote store.PiProviderRec, kind, api *string, approved *[]string) store.PiProviderRec {
	rec := remote
	if local != nil {
		rec.Kind = local.Kind
		rec.APIType = local.APIType
		rec.ApprovedModels = append([]string(nil), local.ApprovedModels...)
		rec.Models = append([]string(nil), local.Models...)
		rec.CatalogCheckedAt = local.CatalogCheckedAt
		rec.CatalogError = local.CatalogError
		rec.CatalogSuspect = local.CatalogSuspect
	}
	if kind != nil {
		rec.Kind = *kind
	}
	if api != nil {
		rec.APIType = *api
	}
	if approved != nil {
		rec.ApprovedModels = cleanedModelIDs(*approved)
	}
	return rec
}

// cleanedModelIDs trims and de-duplicates a synced approved list, keeping the
// stored order (that order is what /model shows). A list that cleans down to
// nothing is still a real value: it means "only the default model".
func cleanedModelIDs(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		id := strings.TrimSpace(v)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// applyProviderPointer writes models.json when the pointed-at secret is
// already in this gateway's vault. Called after secret sync and after the
// pointer itself arrives. It rewrites the whole dashboard stack so a local
// multi-provider profile survives a coordinator sync.
func applyProviderPointer(st *store.Store, dataDir string) {
	p := st.PiProvider()
	if p == nil || p.SecretID == "" || p.BaseURL == "" {
		log.Printf("technocore: pi profile: no provider pointer yet")
		return
	}
	profiles := st.PiProviderStack()
	providers := make([]piprofile.Provider, 0, len(profiles))
	for i := range profiles {
		rec, ok := secretForPointer(st, profiles[i].SecretID)
		if !ok || !strings.HasPrefix(rec.Placeholder, model.PlaceholderPrefix) {
			log.Printf("technocore: pi profile: secret %s not in this vault yet", profiles[i].SecretID)
			continue
		}
		providers = append(providers, piprofile.Provider{
			ID: profiles[i].ID, Name: profiles[i].Name, APIKey: rec.Placeholder,
			BaseURL: profiles[i].BaseURL, API: profiles[i].APIType, Models: profiles[i].Models,
		})
	}
	dir := piProfileDir(dataDir)
	if err := piprofile.WriteProviderStack(dir, providers); err != nil {
		log.Printf("technocore: pi profile: write %s: %v", dir, err)
		return
	}
	providerID, modelID := "", ""
	if active := st.PiProvider(); active != nil {
		providerID = active.ID
		modelID = active.DefaultModel
		if strings.TrimSpace(modelID) == "" {
			for _, m := range active.Models {
				if strings.TrimSpace(m) != "" {
					modelID = m
					break
				}
			}
		}
	}
	if err := piprofile.WriteProviderDefault(dir, providerID, modelID); err != nil {
		log.Printf("technocore: pi profile: default model: %v", err)
		return
	}
	log.Printf("technocore: pi profile: wrote %s (%d providers)", filepath.Join(dir, "models.json"), len(providers))
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
