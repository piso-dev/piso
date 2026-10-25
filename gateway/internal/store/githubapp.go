package store

import (
	"fmt"
	"strings"
	"time"

	"piso/gateway/internal/model"
)

const (
	GithubAppStatusNeedsRegister = "needs-register"
	GithubAppStatusNeedsInstall  = "needs-install"
	GithubAppStatusOK            = "ok"
)

// Github app provenance. A local app was created or imported on this gateway; a
// technocore app was last written by another enrolled gateway. Provenance is
// informational: any gateway may change any app in the group, and the
// coordinator's content_version decides which write wins. Both carry the PEM —
// minting needs it locally, and the key only ever moves between gateways inside
// a sealed envelope.
const (
	GithubAppSourceLocal      = "local"
	GithubAppSourceTechnocore = "technocore"
)

// GithubInstallation is one GitHub account/org the app is installed on.
type GithubInstallation struct {
	AccountID   int64    `json:"accountId,omitempty"`
	ID          int64    `json:"id"`
	Account     string   `json:"account"`
	AccountType string   `json:"accountType,omitempty"`
	HTMLURL     string   `json:"htmlUrl,omitempty"`
	Repos       []string `json:"repos,omitempty"`
}

// GithubAppRec is a gateway-owned GitHub App. The PEM never leaves state.json.
type GithubAppRec struct {
	OwnerID       int64                `json:"ownerId,omitempty"`
	Owner         string               `json:"owner,omitempty"`
	OwnerType     string               `json:"ownerType,omitempty"`
	ID            string               `json:"id"`
	SecretID      string               `json:"secretId"`
	Name          string               `json:"name"`
	Placeholder   string               `json:"placeholder"`
	AppID         int64                `json:"appId,omitempty"`
	Slug          string               `json:"slug,omitempty"`
	PrivateKey    string               `json:"privateKey,omitempty"`
	ClientID      string               `json:"clientId,omitempty"`
	ClientSecret  string               `json:"clientSecret,omitempty"`
	WebhookSecret string               `json:"webhookSecret,omitempty"`
	InstallState  string               `json:"installState,omitempty"`
	StateExpires  time.Time            `json:"stateExpires,omitempty"`
	RefreshError  string               `json:"refreshError,omitempty"`
	ManifestState string               `json:"manifestState,omitempty"`
	Installations []GithubInstallation `json:"installations,omitempty"`
	// Source is empty (or "local") for an app configured here and
	// "technocore" for a mirror of a peer's app. TechnocoreID and
	// ContentVersion are the coordinator resource and version the mirror was
	// built from.
	Source         string    `json:"source,omitempty"`
	TechnocoreID   string    `json:"technocoreId,omitempty"`
	ContentVersion int       `json:"contentVersion,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// SourceOrLocal normalises the empty default to "local".
func (r GithubAppRec) SourceOrLocal() string {
	if r.Source == "" {
		return GithubAppSourceLocal
	}
	return r.Source
}

// Status is the dashboard chip state.
func (r GithubAppRec) Status() string {
	if strings.TrimSpace(r.PrivateKey) == "" || r.AppID == 0 {
		return GithubAppStatusNeedsRegister
	}
	if len(r.Installations) == 0 {
		return GithubAppStatusNeedsInstall
	}
	return GithubAppStatusOK
}

// ToSummary strips PEM and client secrets.
func (r GithubAppRec) ToSummary() model.GithubApp {
	inst := make([]model.GithubInstallationView, 0, len(r.Installations))
	for _, i := range r.Installations {
		inst = append(inst, model.GithubInstallationView{
			ID: i.ID, Account: i.Account, AccountType: i.AccountType,
			HTMLURL: i.HTMLURL, Repos: append([]string(nil), i.Repos...),
		})
	}
	return model.GithubApp{
		ID: r.ID, SecretID: r.SecretID, Name: r.Name, Placeholder: r.Placeholder,
		AppID: r.AppID, Slug: r.Slug, Status: r.Status(),
		Source:  r.SourceOrLocal(),
		OwnerID: r.OwnerID, Owner: r.Owner, OwnerType: r.OwnerType,
		Installations: inst, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (s *Store) GithubApps() []GithubAppRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyGithubApps(s.state.GithubApps)
}

func (s *Store) GithubAppByID(id string) (GithubAppRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.GithubApps {
		if r.ID == id {
			return copyGithubApp(r), true
		}
	}
	return GithubAppRec{}, false
}

func (s *Store) GithubAppByPlaceholder(placeholder string) (GithubAppRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.GithubApps {
		if r.Placeholder == placeholder {
			return copyGithubApp(r), true
		}
	}
	return GithubAppRec{}, false
}

func (s *Store) GithubAppByState(state string) (GithubAppRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if state == "" {
		return GithubAppRec{}, false
	}
	for _, r := range s.state.GithubApps {
		if r.ManifestState == state {
			return copyGithubApp(r), true
		}
	}
	return GithubAppRec{}, false
}

// ConsumeGithubInstallState atomically validates and removes a callback nonce.
func (s *Store) ConsumeGithubInstallState(state string, now time.Time) (GithubAppRec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state != "" {
		for i, rec := range s.state.GithubApps {
			if rec.InstallState == state && rec.StateExpires.After(now) {
				rec.InstallState = ""
				rec.StateExpires = time.Time{}
				s.state.GithubApps[i] = rec
				if err := s.save(); err != nil {
					return GithubAppRec{}, err
				}
				return copyGithubApp(rec), nil
			}
		}
	}
	return GithubAppRec{}, fmt.Errorf("invalid installation state")
}

func (s *Store) GithubAppViews() map[string]model.GithubAppView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]model.GithubAppView{}
	for _, r := range s.state.GithubApps {
		placeholder := ""
		for _, sec := range s.state.Secrets {
			if sec.ID == r.SecretID && sec.Kind == model.SecretKindGithubApp && !sec.GatewayOnly {
				placeholder = sec.Placeholder
				break
			}
		}
		if placeholder == "" {
			continue
		}
		accounts := make([]string, 0, len(r.Installations))
		for _, i := range r.Installations {
			if i.Account != "" {
				accounts = append(accounts, strings.ToLower(i.Account))
			}
		}
		view := out[placeholder]
		if strings.TrimSpace(r.PrivateKey) != "" && r.AppID != 0 {
			view.HasPEM = true
			view.Accounts = append(view.Accounts, accounts...)
		}
		out[placeholder] = view
	}
	return out
}

func (s *Store) UpsertGithubApp(rec GithubAppRec) (GithubAppRec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.ID == "" {
		return GithubAppRec{}, fmt.Errorf("github app id required")
	}
	linked := false
	for _, sec := range s.state.Secrets {
		if sec.ID == rec.SecretID && sec.Kind == model.SecretKindGithubApp {
			rec.Placeholder = sec.Placeholder
			linked = true
			break
		}
	}
	if !linked {
		return GithubAppRec{}, fmt.Errorf("github app requires a github-app secret")
	}
	rec.UpdatedAt = time.Now().UTC()
	for i, existing := range s.state.GithubApps {
		if existing.ID == rec.ID {
			s.state.GithubApps[i] = rec
			if err := s.save(); err != nil {
				return GithubAppRec{}, err
			}
			s.broadcastGithub(rec)
			return copyGithubApp(rec), nil
		}
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = rec.UpdatedAt
	}
	s.state.GithubApps = append(s.state.GithubApps, rec)
	if err := s.save(); err != nil {
		return GithubAppRec{}, err
	}
	s.broadcastGithub(rec)
	return copyGithubApp(rec), nil
}

// GithubAppsBySecretID returns every app attached to one routing group.
func (s *Store) GithubAppsBySecretID(secretID string) []GithubAppRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []GithubAppRec
	for _, r := range s.state.GithubApps {
		if r.SecretID == secretID {
			out = append(out, copyGithubApp(r))
		}
	}
	return out
}

// GithubAppByAppID finds an app by its GitHub App id, local or mirrored.
func (s *Store) GithubAppByAppID(appID int64) (GithubAppRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.GithubApps {
		if r.AppID == appID && appID != 0 {
			return copyGithubApp(r), true
		}
	}
	return GithubAppRec{}, false
}

// DeleteGithubAppRecord removes one app record and leaves its routing group
// alone, so a sync that drops an app does not take the group's secret with it.
func (s *Store) DeleteGithubAppRecord(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state.GithubApps[:0]
	removed := false
	for _, r := range s.state.GithubApps {
		if r.ID == id {
			removed = true
			continue
		}
		out = append(out, r)
	}
	if !removed {
		return nil
	}
	s.state.GithubApps = out
	if err := s.save(); err != nil {
		return err
	}
	return nil
}

func (s *Store) DeleteGithubApp(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var secretID string
	out := s.state.GithubApps[:0]
	for _, r := range s.state.GithubApps {
		if r.ID == id {
			secretID = r.SecretID
			continue
		}
		out = append(out, r)
	}
	s.state.GithubApps = out
	for _, app := range out {
		if app.SecretID == secretID {
			secretID = ""
			break
		}
	}
	if secretID != "" {
		secs := s.state.Secrets[:0]
		for _, rec := range s.state.Secrets {
			if rec.ID != secretID {
				secs = append(secs, rec)
			}
		}
		s.state.Secrets = secs
		s.state.Rules = filterRules(s.state.Rules, func(r RuleRec) bool { return r.SecretID != secretID })
	}
	if err := s.save(); err != nil {
		return err
	}
	return s.writePlaceholdersEnvLocked()
}

func (s *Store) PendingGithubApps() []GithubAppRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []GithubAppRec
	for _, r := range s.state.GithubApps {
		if r.Status() != GithubAppStatusOK {
			out = append(out, copyGithubApp(r))
		}
	}
	return out
}

func (s *Store) broadcastGithub(rec GithubAppRec) {
	select {
	case s.onGithub <- rec:
	default:
	}
}

func (s *Store) SubGithub() <-chan GithubAppRec { return s.onGithub }

func copyGithubApps(in []GithubAppRec) []GithubAppRec {
	out := make([]GithubAppRec, len(in))
	for i, r := range in {
		out[i] = copyGithubApp(r)
	}
	return out
}

func copyGithubApp(r GithubAppRec) GithubAppRec {
	r.Installations = append([]GithubInstallation(nil), r.Installations...)
	for i := range r.Installations {
		r.Installations[i].Repos = append([]string(nil), r.Installations[i].Repos...)
	}
	return r
}
