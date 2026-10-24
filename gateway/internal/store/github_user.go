package store

// Where a GitHub sign-in came from. A local sign-in is this gateway's own; a
// technocore sign-in was configured on another enrolled gateway and is a
// mirror of it.
const (
	GithubUserSourceLocal      = "local"
	GithubUserSourceTechnocore = "technocore"
)

// GithubUserRec contains identity and vault references, never OAuth credentials.
type GithubUserRec struct {
	ClientID       string `json:"clientId,omitempty"`
	ClientSecretID string `json:"clientSecretId,omitempty"`
	TokenSecretID  string `json:"tokenSecretId,omitempty"`
	Login          string `json:"login,omitempty"`
	AccountID      int64  `json:"accountId,omitempty"`
	// Scopes is the space-separated scope list GitHub granted at sign-in.
	// Empty means it was never recorded (an older connection); callers must
	// treat that as unknown, not as "no scopes".
	Scopes string `json:"scopes,omitempty"`
	// Source is "local" or "technocore". A technocore sign-in is read-only
	// here in the sense that its origin republishes it; signing out is still
	// global (it tombstones the shared session for every gateway).
	Source string `json:"source,omitempty"`
	// TechnocoreID and ContentVersion pin the shared session this gateway is
	// mirroring, so an older envelope can never roll a newer one back.
	TechnocoreID   string `json:"technocoreId,omitempty"`
	ContentVersion int    `json:"contentVersion,omitempty"`
	// BundleID is the vault row that carries this session between gateways:
	// the one this gateway created when it signed in, or the mirrored row it
	// received. Sharing is decided per row against this id, so a gateway that
	// both signed in and holds a peer's session never republishes the peer's
	// row with its own token.
	BundleID string `json:"bundleId,omitempty"`
}

// SourceOrLocal reports the source, defaulting an unset value to local: rows
// written before sharing existed were all signed in here.
func (r GithubUserRec) SourceOrLocal() string {
	if r.Source == "" {
		return GithubUserSourceLocal
	}
	return r.Source
}

func (s *Store) GithubUser() GithubUserRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.GithubUser
}
func (s *Store) SetGithubUser(r GithubUserRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.GithubUser = r
	return s.save()
}
