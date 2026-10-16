package store

// GithubUserRec contains identity and vault references, never OAuth credentials.
type GithubUserRec struct {
	ClientID       string `json:"clientId,omitempty"`
	ClientSecretID string `json:"clientSecretId,omitempty"`
	TokenSecretID  string `json:"tokenSecretId,omitempty"`
	Login          string `json:"login,omitempty"`
	AccountID      int64  `json:"accountId,omitempty"`
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
