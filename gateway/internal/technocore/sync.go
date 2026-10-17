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
	Status         string         `json:"status"`
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
	rec := newReconciler(st, dataDir)
	go rec.loop()
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
		ready, err := connectSession(id, dataDir, rec.Nudge, func(ev string) {
			switch ev {
			case "asks_changed":
				st.BroadcastAsks()
			case "telegram_changed":
				// The allow-list and claim code belong to the coordinator. Wake
				// the poller so it refetches instead of enforcing a stale list
				// until its next renewal.
				st.BroadcastTelegram()
			}
		})
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
	res, err := serverHTTP.Do(req)
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

func syncAssignedWorkers(st *store.Store, id tc.Identity) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(id.ServerURL, "/")+"/api/v1/sync/workers", nil)
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
	if res.StatusCode != 200 {
		return fmt.Errorf("sync workers HTTP %d: %s", res.StatusCode, raw)
	}
	var out struct {
		Workers []struct {
			PrincipalID string `json:"principal_id"`
			Name        string `json:"name"`
			Pubkey      string `json:"pubkey"`
			Slug        string `json:"slug"`
			TokenHash   string `json:"token_hash"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	rows := make([]store.WorkerRec, 0, len(out.Workers))
	for _, w := range out.Workers {
		slug := strings.TrimSpace(w.Slug)
		if slug == "" {
			slug = strings.TrimSpace(w.Name)
		}
		name := strings.TrimSpace(w.Name)
		if name == "" {
			name = "piso-worker-" + slug
		}
		if !strings.HasPrefix(name, "piso-worker-") {
			name = "piso-worker-" + slug
		}
		rows = append(rows, store.WorkerRec{
			Name:        name,
			Slug:        slug,
			TokenHash:   w.TokenHash,
			Pubkey:      w.Pubkey,
			PrincipalID: w.PrincipalID,
			Remote:      true,
		})
	}
	return st.ReplaceRemoteWorkers(rows)
}

// syncSecrets replaces the local tc_ snapshot with a complete coordinator
// response. A non-200, a transport error, or a body that does not parse
// returns before any delete, so a restart blip cannot wipe the vault.
func syncSecrets(st *store.Store, dataDir string, id tc.Identity) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(id.ServerURL, "/")+"/api/v1/sync/secrets", nil)
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
		kept, err := applyEnvelope(st, id, obj)
		if err != nil {
			log.Printf("technocore: skip %s: %v", obj.ResourceID, err)
			skipped++
			continue
		}
		seen[kept] = true
		applied++
	}
	// The pointer may already be stored from an earlier pull. Apply it now
	// that this gateway's copy of the secret is in the vault.
	applyProviderPointer(st, dataDir)
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
	now := time.Now().UTC()
	setLive(func(s *Status) {
		s.LastSync = &now
		s.LastError = ""
		s.EnvelopeSkip = skipped
	})
	if applied > 0 || removed > 0 {
		log.Printf("technocore: applied %d envelopes, removed %d", applied, removed)
	}
	return nil
}

// statusRevoked is the coordinator's tombstone marker. A revoked resource is
// still delivered with a bumped content_version so every gateway learns about
// the deletion instead of inferring it from silence.
const statusRevoked = "revoked"

func applyEnvelope(st *store.Store, id tc.Identity, obj envelope) (string, error) {
	if obj.Algorithm != "" && obj.Algorithm != tc.EnvelopeAlg {
		return "", fmt.Errorf("unsupported algorithm %q", obj.Algorithm)
	}
	ct, err := base64.StdEncoding.DecodeString(obj.Ciphertext)
	if err != nil {
		return "", err
	}
	nonce, err := base64.StdEncoding.DecodeString(obj.Nonce)
	if err != nil {
		return "", err
	}
	wrap, err := base64.StdEncoding.DecodeString(obj.WrappedDataKey)
	if err != nil {
		return "", err
	}
	plaintext, err := tc.OpenEnvelope(id, wrap, nonce, ct)
	if err != nil {
		return "", err
	}
	gatewayOnly, _ := obj.Metadata["gatewayOnly"].(bool)
	// The subtype ("telegram", ...) rides in metadata: the coordinator stores
	// every envelope as a secret resource, so this is the only place it can
	// travel. Empty means the envelope predates it, not that the secret has no
	// subtype.
	kind, _ := obj.Metadata["kind"].(string)
	placeholder, _ := obj.Metadata["placeholder"].(string)
	if placeholder == "" && !gatewayOnly {
		// Gateway-only credentials are never substituted into worker traffic,
		// so minting a placeholder for them would only invent a token that no
		// rule should ever reference.
		idhex := strings.ReplaceAll(obj.ResourceID, "-", "")
		if len(idhex) > 16 {
			idhex = idhex[:16]
		}
		placeholder = "piso_tc_" + idhex
	}
	envKey, _ := obj.Metadata["envKey"].(string)
	hosts := stringSlice(obj.Metadata["allowedHosts"])
	secretID := "tc_" + obj.ResourceID
	plain := string(plaintext)
	hash := plaintextHash(plain)

	local, found := matchLocalSecret(st, obj.ResourceID, secretID, placeholder, plain)
	if found {
		if obj.Status == statusRevoked {
			// A tombstone is an ordinary version-ordered change: it wins unless
			// the local copy is strictly ahead, which means this envelope is
			// stale.
			if obj.ContentVersion >= local.ContentVersion {
				return local.ID, st.RevokeSecret(local.ID, obj.ContentVersion)
			}
			return local.ID, nil
		}
		if obj.ContentVersion < local.ContentVersion {
			// Never roll a newer local copy back.
			return local.ID, nil
		}
		if obj.ContentVersion == local.ContentVersion && (local.Value != plain || local.Revoked) {
			// Same version, different bytes: the coordinator has not seen a
			// write at this version, so the local copy is an edit that has not
			// been published yet. Keep it; publishAllSecrets resolves the race.
			return local.ID, nil
		}
		// The coordinator's version is authoritative, so the plaintext and the
		// worker-facing flags come from the envelope either way.
		local.Value = plain
		local.AllowedHosts = hosts
		local.GatewayOnly = gatewayOnly
		if kind != "" {
			// Follow the value: the subtype describes the plaintext. An envelope
			// with no subtype must not erase one learned earlier.
			local.Kind = kind
		}
		if local.ID == secretID {
			// Coordinator-owned row: refresh identity metadata too.
			local.Name = obj.Name
			local.Placeholder = placeholder
			local.EnvKey = envKey
		}
		local.Revoked = false
		// Local-origin rows keep their id and placeholder so workers and rules stay valid.
		if local.TechnocoreID == "" {
			local.TechnocoreID = obj.ResourceID
		}
		if local.PublishedHash == "" {
			local.PublishedHash = hash
		}
		local.ContentVersion = obj.ContentVersion
		if err := st.ReplaceSecret(local); err != nil {
			return "", err
		}
		return local.ID, st.SyncRulesForSecret(local)
	}

	rec := store.SecretRec{
		ID:             secretID,
		Name:           obj.Name,
		Kind:           kind,
		Placeholder:    placeholder,
		Value:          plain,
		EnvKey:         envKey,
		AllowedHosts:   hosts,
		GatewayOnly:    gatewayOnly,
		CreatedAt:      time.Now().UTC(),
		TechnocoreID:   obj.ResourceID,
		PublishedHash:  hash,
		ContentVersion: obj.ContentVersion,
	}
	if obj.Status == statusRevoked {
		rec.Value = ""
		rec.Revoked = true
	}
	if err := st.AddSecret(rec); err != nil {
		return "", err
	}
	return rec.ID, st.SyncRulesForSecret(rec)
}

// matchLocalSecret finds a vault row that is the same coordinator secret:
// stamped resource id, synced tc_ id, placeholder, or identical plaintext.
// Matching by value is what stops independently minted piso_ suffixes from
// becoming a second local row after inbound sync.
func matchLocalSecret(st *store.Store, resourceID, secretID, placeholder, plain string) (store.SecretRec, bool) {
	if rec, ok := st.SecretByTechnocoreID(resourceID); ok {
		return rec, true
	}
	if rec, ok := st.SecretByID(secretID); ok {
		return rec, true
	}
	if placeholder != "" {
		if rec, ok := st.SecretByPlaceholder(placeholder); ok {
			return rec, true
		}
	}
	if plain == "" {
		return store.SecretRec{}, false
	}
	for _, rec := range st.Secrets() {
		if rec.Value == plain {
			return rec, true
		}
	}
	return store.SecretRec{}, false
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
