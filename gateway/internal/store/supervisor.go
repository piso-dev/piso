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
	// LastMailboxPoll is runtime-only: a gateway restart must re-confirm readiness.
	LastMailboxPoll time.Time `json:"-"`
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
	now := time.Now().UTC()
	previous := s.state.Supervisor
	r.LastMailboxPoll = time.Time{}
	if previous.PrincipalID == r.PrincipalID && previous.Worker == r.Worker && previous.ExpiresAt.After(now) && r.ExpiresAt.After(now) {
		r.LastMailboxPoll = previous.LastMailboxPoll
	}
	r.UpdatedAt = now
	s.state.Supervisor = r
	return s.save()
}

// MarkSupervisorMailboxPoll records a completed poll only for the live holder.
// It stays in memory; lease renewal must not manufacture mailbox readiness.
func (s *Store) MarkSupervisorMailboxPoll(worker string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Supervisor.Worker != worker || s.state.Supervisor.PrincipalID == "" || !s.state.Supervisor.ExpiresAt.After(now) {
		return false
	}
	s.state.Supervisor.LastMailboxPoll = now.UTC()
	return true
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
