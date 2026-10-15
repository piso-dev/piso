package store

import "time"

// SupervisorLink is the worker the coordinator named as supervisor of this
// gateway's Telegram front door. It is a cache of the coordinator's lease: the
// gateway writes it when a worker's ask is granted and reads it to answer the
// dashboard and to route a message. An entry whose ExpiresAt has passed is
// treated as absent, so the gateway never has to clear it on time.
type SupervisorLink struct {
	Worker      string    `json:"worker,omitempty"`      // worker container name
	PrincipalID string    `json:"principalId,omitempty"` // coordinator worker principal UUID
	Slug        string    `json:"slug,omitempty"`
	ExpiresAt   time.Time `json:"expiresAt,omitempty"`
	// Reason is the last refusal for this worker, for the dashboard. A refusal
	// clears ExpiresAt, so a refused worker is never a live supervisor.
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// SupervisorLink returns a copy of the cached supervisor, live or not.
func (s *Store) SupervisorLink() SupervisorLink {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Supervisor
}

// SetSupervisorLink replaces the cached supervisor.
func (s *Store) SetSupervisorLink(r SupervisorLink) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.UpdatedAt = time.Now().UTC()
	s.state.Supervisor = r
	return s.save()
}

// Supervisor returns the live supervisor, or ok=false when none is cached or
// the cached lease has expired.
func (s *Store) Supervisor(now time.Time) (SupervisorLink, bool) {
	link := s.SupervisorLink()
	if link.PrincipalID == "" || !link.ExpiresAt.After(now) {
		return SupervisorLink{}, false
	}
	return link, true
}
