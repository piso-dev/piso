package technocore

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"piso/gateway/internal/githubapp"
	"piso/gateway/internal/model"
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
			now := time.Now().UTC()
			setLive(func(s *Status) {
				s.LastError = err.Error()
				if s.OfflineSince == nil {
					s.OfflineSince = &now
				}
			})
		}
		if ready {
			backoff = time.Second
			setLive(func(s *Status) {
				s.OfflineSince = nil
				s.LastError = ""
			})
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

// tickPending advances the pairing state machine. The phone's approve only
// moves the request to awaiting_gateway; this process never confirms on its
// own, because the confirm is the gateway operator's consent. Each branch
// writes only the pairing fields through tc.Update so a concurrent CLI write
// (placement, public URL) is not clobbered by a stale copy.
func tickPending(path string, id tc.Identity) error {
	state, err := tc.PollPairing(id)
	if err != nil {
		if !errors.Is(err, tc.ErrPollToken) {
			return err
		}
		if _, err := tc.Update(path, func(cur *tc.Identity) error {
			if cur.PairingID != id.PairingID {
				// A newer attempt replaced this one; leave it alone.
				return nil
			}
			resetPairFields(cur)
			return nil
		}); err != nil {
			return err
		}
		setLive(func(s *Status) {
			s.LastError = "the pairing request is no longer recognised; show a new code"
		})
		log.Printf("technocore: pairing token rejected; attempt cleared")
		return nil
	}

	_, err = tc.Update(path, func(cur *tc.Identity) error {
		if cur.PairingID != id.PairingID {
			return nil
		}
		switch state.Status {
		case tc.PairStatusPending:
			cur.PairPhase = tc.PairStatusPending
			cur.PairExpiresAt = state.ExpiresAt
			cur.PairApprover = ""
			cur.PairAccount = ""
		case tc.PairStatusAwaitingGateway:
			if cur.PairPhase != tc.PairStatusAwaitingGateway {
				log.Printf("technocore: phone approved; waiting for the dashboard confirm")
			}
			cur.PairPhase = tc.PairStatusAwaitingGateway
			cur.PairApprover = state.ApprovedBy
			cur.PairAccount = state.AccountName
			cur.PairExpiresAt = state.ExpiresAt
		case tc.PairStatusApproved:
			// Confirmed from this dashboard (or from `piso gateway launch
			// --yes`). The principal already exists on the coordinator.
			cur.Status = tc.StatusActive
			cur.PrincipalID = state.PrincipalID
			cur.PairPhase = ""
			cur.PairExpiresAt = ""
			cur.PairApprover = ""
			cur.PairAccount = ""
			log.Printf("technocore: pairing confirmed (%s)", state.PrincipalID)
		case tc.PairStatusRejected:
			resetPairFields(cur)
			setLive(func(s *Status) { s.LastError = "the pairing was declined" })
		case tc.PairStatusExpired:
			resetPairFields(cur)
			setLive(func(s *Status) { s.LastError = "the pairing code expired" })
		}
		return nil
	})
	return err
}

// resetPairFields returns an unenrolled identity to a clean slate while
// keeping the keys and the coordinator URL, so the next attempt reuses the
// same fingerprint.
func resetPairFields(cur *tc.Identity) {
	if cur.Status == tc.StatusActive {
		return
	}
	cur.Status = tc.StatusNew
	cur.PairingID = ""
	cur.PollToken = ""
	cur.PairPhase = ""
	cur.PairExpiresAt = ""
	cur.PairApprover = ""
	cur.PairAccount = ""
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

// Pull applies the coordinator's current envelopes now. A writer that lost a
// version race uses it to rebase: the coordinator's snapshot is authoritative,
// so the caller re-applies its change to what the other gateway published
// instead of overwriting it.
func Pull(st *store.Store, dataDir string) error {
	id, err := tc.Load(tc.Path(dataDir))
	if err != nil {
		return fmt.Errorf("not enrolled: %w", err)
	}
	if id.Status != tc.StatusActive {
		return fmt.Errorf("gateway is not enrolled")
	}
	return syncSecrets(st, dataDir, id)
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

// appliedEnvelope reports what an envelope did to the vault, so the GitHub App
// mirror can follow it: a github-app row carries the app bundle in its sealed
// plaintext instead of a credential.
type appliedEnvelope struct {
	RowID   string
	Kind    string
	Plain   string
	Applied bool
}

// applyEnvelope writes the vault row, then materialises any GitHub Apps the
// envelope carried. App mirroring is best effort: a bundle this gateway cannot
// parse must not block the secret row itself.
func applyEnvelope(st *store.Store, id tc.Identity, obj envelope) (string, error) {
	res, err := applySecretEnvelope(st, id, obj)
	if err != nil {
		return "", err
	}
	switch res.Kind {
	case model.SecretKindGithubApp:
		applySharedGithubApps(st, obj, res)
	case model.SecretKindGithubUser:
		// The dashboard sign-in that lets someone manage apps, shared so a
		// person signs in once for the whole account instead of once per
		// gateway.
		if obj.Status == statusRevoked {
			clearSharedGithubUser(st)
		} else {
			applySharedGithubUser(st, obj, res)
		}
	}
	return res.RowID, nil
}

// applySharedGithubUser materializes the shared GitHub sign-in as this
// gateway's own gateway-only rows, so every existing reader (status, authorize,
// verifiedGithubUser) works unchanged. Any gateway may publish a session, so the
// newest version wins and signing in anywhere replaces the session everywhere;
// there is no per-gateway copy to protect.
func applySharedGithubUser(st *store.Store, obj envelope, res appliedEnvelope) {
	if !res.Applied {
		return
	}
	u := st.GithubUser()
	if obj.ContentVersion < u.ContentVersion {
		// Never roll a newer session back to an older one.
		return
	}
	bundle, err := githubapp.DecodeUserBundle(res.Plain)
	if err != nil {
		log.Printf("technocore: github sign-in bundle for %s: %v", res.RowID, err)
		return
	}
	// The token and client secret are stored as ordinary gateway-only rows on
	// this gateway, reusing this gateway's existing rows so a resync updates
	// them instead of accumulating copies.
	clientSecretID := upsertGithubOAuthRow(st, u.ClientSecretID, "GitHub OAuth client secret", bundle.ClientSecret)
	tokenID := upsertGithubOAuthRow(st, u.TokenSecretID, "GitHub user OAuth — gateway only", bundle.Token)
	if tokenID == "" {
		log.Printf("technocore: could not store shared GitHub sign-in %s", res.RowID)
		return
	}
	next := store.GithubUserRec{
		ClientID:       bundle.ClientID,
		ClientSecretID: clientSecretID,
		TokenSecretID:  tokenID,
		Login:          bundle.Login,
		AccountID:      bundle.AccountID,
		Scopes:         bundle.Scopes,
		Source:         store.GithubUserSourceTechnocore,
		TechnocoreID:   obj.ResourceID,
		ContentVersion: obj.ContentVersion,
		BundleID:       res.RowID,
	}
	if err := st.SetGithubUser(next); err != nil {
		log.Printf("technocore: store shared GitHub sign-in: %v", err)
	}
}

// localID returns a short random suffix for a materialized row.
func localID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// upsertGithubOAuthRow writes one part of a shared sign-in, replacing the row
// this gateway already used when it exists. An empty value (a bundle from a
// gateway that never had the client secret) leaves the rows alone.
func upsertGithubOAuthRow(st *store.Store, id, name, value string) string {
	if value == "" {
		return id
	}
	if rec, ok := st.SecretByID(id); ok && rec.Kind == model.SecretKindGithubOAuth && !rec.Revoked {
		rec.Value = value
		rec.GatewayOnly = true
		if err := st.ReplaceSecret(rec); err == nil {
			return rec.ID
		}
	}
	rec := store.SecretRec{
		ID:          "sec_" + localID(),
		Name:        name,
		Kind:        model.SecretKindGithubOAuth,
		Placeholder: "piso_github_oauth_" + localID(),
		Value:       value,
		GatewayOnly: true,
		CreatedAt:   time.Now().UTC(),
	}
	if err := st.AddSecret(rec); err != nil {
		log.Printf("technocore: store github-oauth %s: %v", name, err)
		return ""
	}
	return rec.ID
}

// clearSharedGithubUser drops the session a gateway signed out of. Sign-out is
// global — one account session — so it applies here whether this gateway signed
// in itself or received the shared copy. The OAuth App configuration survives:
// signing back in needs no re-setup.
func clearSharedGithubUser(st *store.Store) {
	u := st.GithubUser()
	if u.TokenSecretID != "" {
		_ = st.DeleteSecret(u.TokenSecretID)
	}
	// The OAuth App configuration stays — client id and client secret rows are
	// what let the next sign-in skip re-setup on every gateway.
	// Drop the transport row too: its coordinator resource is the tombstone this
	// envelope carries, so keeping it would only hand the next sign-in a dead row
	// to republish into.
	for _, rec := range st.Secrets() {
		if rec.Kind == model.SecretKindGithubUser {
			_ = st.DeleteSecret(rec.ID)
		}
	}
	if err := st.SetGithubUser(store.GithubUserRec{ClientID: u.ClientID, ClientSecretID: u.ClientSecretID}); err != nil {
		log.Printf("technocore: clear shared GitHub sign-in: %v", err)
	}
}

// applySharedGithubApps rebuilds this gateway's copy of a routing group from
// the envelope. The envelope is authoritative: every gateway may publish, so a
// row's provenance records where it was configured, not whether it may be
// changed here. Apps are matched by GitHub App ID, so an app created on this
// gateway and then edited on another converges instead of diverging.
func applySharedGithubApps(st *store.Store, obj envelope, res appliedEnvelope) {
	if obj.Status == statusRevoked {
		for _, app := range st.GithubAppsBySecretID(res.RowID) {
			if err := st.DeleteGithubAppRecord(app.ID); err != nil {
				log.Printf("technocore: drop github app %s: %v", app.ID, err)
			}
		}
		return
	}
	if !res.Applied {
		return
	}
	bundle, err := githubapp.DecodeBundle(res.Plain)
	if err != nil {
		log.Printf("technocore: github app bundle for %s: %v", res.RowID, err)
		return
	}
	wanted := make(map[int64]bool, len(bundle.Apps))
	for _, rec := range bundle.Recs(res.RowID, obj.ResourceID, obj.ContentVersion) {
		wanted[rec.AppID] = true
		if existing, ok := st.GithubAppByAppID(rec.AppID); ok {
			// Keep this gateway's row id so dashboard links and in-flight
			// flows survive an update that originated elsewhere.
			rec.ID = existing.ID
			rec.CreatedAt = existing.CreatedAt
			// These are not shared: an install or manifest started here must
			// survive a sync that lands while the person is still at GitHub,
			// and a discovery warning describes GitHub, not the bundle.
			rec.InstallState = existing.InstallState
			rec.StateExpires = existing.StateExpires
			rec.ManifestState = existing.ManifestState
			rec.RefreshError = existing.RefreshError
		}
		if _, err := st.UpsertGithubApp(rec); err != nil {
			log.Printf("technocore: apply github app %d: %v", rec.AppID, err)
		}
	}
	// The bundle at this version is the whole group, so an app it no longer
	// lists was removed on whichever gateway published last. A record this
	// gateway has never seen published (version 0) is an app minted here that
	// has not reached the coordinator yet — including one still mid-manifest —
	// so it is left for the reconcile pass to share rather than dropped.
	for _, app := range st.GithubAppsBySecretID(res.RowID) {
		if wanted[app.AppID] || app.ContentVersion == 0 {
			continue
		}
		if err := st.DeleteGithubAppRecord(app.ID); err != nil {
			log.Printf("technocore: drop github app %s: %v", app.ID, err)
		}
	}
}

func applySecretEnvelope(st *store.Store, id tc.Identity, obj envelope) (appliedEnvelope, error) {
	if obj.Algorithm != "" && obj.Algorithm != tc.EnvelopeAlg {
		return appliedEnvelope{}, fmt.Errorf("unsupported algorithm %q", obj.Algorithm)
	}
	ct, err := base64.StdEncoding.DecodeString(obj.Ciphertext)
	if err != nil {
		return appliedEnvelope{}, err
	}
	nonce, err := base64.StdEncoding.DecodeString(obj.Nonce)
	if err != nil {
		return appliedEnvelope{}, err
	}
	wrap, err := base64.StdEncoding.DecodeString(obj.WrappedDataKey)
	if err != nil {
		return appliedEnvelope{}, err
	}
	plaintext, err := tc.OpenEnvelope(id, wrap, nonce, ct)
	if err != nil {
		return appliedEnvelope{}, err
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
	// A github-app envelope carries the app bundle, not a credential: the vault
	// row stays empty so the PEM is never revealed by the secret UI, and the
	// content_version alone decides which copy is authoritative.
	// Kinds whose plaintext is a structured bundle rather than a credential:
	// the vault row stays empty (the materialized records hold the material)
	// and a same-version mismatch is not a local edit to preserve.
	sharesApps := kind == model.SecretKindGithubApp || kind == model.SecretKindGithubUser

	local, found := matchLocalSecret(st, obj.ResourceID, secretID, placeholder, plain)
	if found {
		if obj.Status == statusRevoked {
			// A tombstone is an ordinary version-ordered change: it wins unless
			// the local copy is strictly ahead, which means this envelope is
			// stale.
			if obj.ContentVersion >= local.ContentVersion {
				return appliedEnvelope{RowID: local.ID, Kind: kind, Applied: true}, st.RevokeSecret(local.ID, obj.ContentVersion)
			}
			return appliedEnvelope{RowID: local.ID, Kind: kind}, nil
		}
		if obj.ContentVersion < local.ContentVersion {
			// Never roll a newer local copy back.
			return appliedEnvelope{RowID: local.ID, Kind: kind}, nil
		}
		if !sharesApps && obj.ContentVersion == local.ContentVersion && (local.Value != plain || local.Revoked) {
			// Same version, different bytes: the coordinator has not seen a
			// write at this version, so the local copy is an edit that has not
			// been published yet. Keep it; publishAllSecrets resolves the race.
			return appliedEnvelope{RowID: local.ID, Kind: kind}, nil
		}
		// The coordinator's version is authoritative, so the plaintext and the
		// worker-facing flags come from the envelope either way.
		local.Value = plain
		if sharesApps {
			// The bundle lives in the app records below, not in the vault.
			local.Value = ""
		}
		if len(hosts) > 0 || len(local.AllowedHosts) == 0 {
			// An envelope with no host list must not erase a binding this
			// gateway already knows: destination binding is now mandatory, and
			// an older coordinator (or a peer that predates it) sends no hosts,
			// so blanking them would silently unbind a working secret.
			local.AllowedHosts = hosts
		}
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
		// Any gateway may publish, so adopt the coordinator's hash: a gateway
		// that received a bundle it did not change must not republish it and
		// bounce content_version back and forth.
		local.PublishedHash = hash
		local.ContentVersion = obj.ContentVersion
		if err := st.ReplaceSecret(local); err != nil {
			return appliedEnvelope{}, err
		}
		if err := st.SyncRulesForSecret(local); err != nil {
			return appliedEnvelope{RowID: local.ID, Kind: kind, Plain: plain, Applied: true}, err
		}
		return appliedEnvelope{RowID: local.ID, Kind: kind, Plain: plain, Applied: true}, nil
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
	if rec.Kind == model.SecretKindGithubApp || rec.Kind == model.SecretKindGithubUser {
		rec.Value = ""
	}
	if err := st.AddSecret(rec); err != nil {
		return appliedEnvelope{}, err
	}
	if err := st.SyncRulesForSecret(rec); err != nil {
		return appliedEnvelope{RowID: rec.ID, Kind: kind, Plain: plain, Applied: true}, err
	}
	return appliedEnvelope{RowID: rec.ID, Kind: kind, Plain: plain, Applied: true}, nil
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
