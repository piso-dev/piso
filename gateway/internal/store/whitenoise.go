package store

import (
	"errors"
	"time"
)

// WhiteNoiseLink contains public metadata and piso authorization only. Private
// identity/MLS state remains in the connector's gateway-only home.
type WhiteNoiseUser struct {
	PublicKey string    `json:"publicKey"`
	Name      string    `json:"name"`
	LinkedAt  time.Time `json:"linkedAt"`
}
type WhiteNoiseConversation struct {
	ID     string `json:"id"`
	Sender string `json:"sender"`
}
type WhiteNoiseLink struct {
	ResourceID    string                   `json:"resourceId"`
	AccountID     string                   `json:"accountId"`
	PublicKey     string                   `json:"publicKey"`
	Name          string                   `json:"name"`
	Enabled       bool                     `json:"enabled"`
	CreatedAt     time.Time                `json:"createdAt"`
	Users         []WhiteNoiseUser         `json:"users"`
	Conversations []WhiteNoiseConversation `json:"conversations,omitempty"`
	Seen          []string                 `json:"seen,omitempty"`
	Mailbox       []WhiteNoiseMail         `json:"mailbox,omitempty"`
	Supervisor    SupervisorLink           `json:"supervisor,omitempty"`
}
type WhiteNoiseMail struct {
	ID             string    `json:"id"`
	Channel        string    `json:"channel"`
	ConnectionID   string    `json:"connectionId"`
	ConversationID string    `json:"conversationId"`
	SenderID       string    `json:"senderId"`
	Text           string    `json:"text"`
	State          MailState `json:"state"`
	Received       time.Time `json:"receivedAt"`
	DeliveredAt    time.Time `json:"deliveredAt,omitempty"`
}

func (s *Store) WhiteNoiseLink() WhiteNoiseLink {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.state.WhiteNoise
	r.Users = append([]WhiteNoiseUser{}, r.Users...)
	r.Conversations = append([]WhiteNoiseConversation{}, r.Conversations...)
	r.Seen = append([]string{}, r.Seen...)
	r.Mailbox = append([]WhiteNoiseMail{}, r.Mailbox...)
	return r
}
func (s *Store) SetWhiteNoiseLink(r WhiteNoiseLink) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.state.WhiteNoise
	if old.AccountID != "" && old.AccountID != r.AccountID {
		return errors.New("identity replacement is not supported")
	}
	if old.AccountID == r.AccountID {
		r.Conversations = old.Conversations
		r.Seen = old.Seen
		r.Mailbox = old.Mailbox
		r.Supervisor = old.Supervisor
	}
	s.state.WhiteNoise = r
	return s.save()
}
func (s *Store) SetWhiteNoiseSupervisor(r SupervisorLink) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.state.WhiteNoise.Supervisor
	if prev.Worker == r.Worker && prev.ExpiresAt.After(time.Now()) {
		r.LastMailboxPoll = prev.LastMailboxPoll
	}
	s.state.WhiteNoise.Supervisor = r
	return s.save()
}
func (s *Store) WhiteNoiseSupervisor(worker string) bool {
	r := s.WhiteNoiseLink()
	return r.Enabled && r.Supervisor.Worker == worker && r.Supervisor.PrincipalID != "" && r.Supervisor.ExpiresAt.After(time.Now())
}
func (s *Store) SetWhiteNoiseEnabled(enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.WhiteNoise.Enabled = enabled
	if !enabled {
		s.state.WhiteNoise.Supervisor = SupervisorLink{}
	}
	return s.save()
}
func (s *Store) SetWhiteNoiseUsers(users []WhiteNoiseUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &s.state.WhiteNoise
	r.Users = append([]WhiteNoiseUser{}, users...)
	allowed := map[string]bool{}
	for _, u := range users {
		allowed[u.PublicKey] = true
	}
	var conv []WhiteNoiseConversation
	for _, c := range r.Conversations {
		if allowed[c.Sender] {
			conv = append(conv, c)
		}
	}
	r.Conversations = conv
	var mail []WhiteNoiseMail
	for _, m := range r.Mailbox {
		if allowed[m.SenderID] {
			mail = append(mail, m)
		}
	}
	r.Mailbox = mail
	return s.save()
}
func (s *Store) WhiteNoiseAllowed(sender string) bool {
	r := s.WhiteNoiseLink()
	if !r.Enabled {
		return false
	}
	for _, u := range r.Users {
		if u.PublicKey == sender {
			return true
		}
	}
	return false
}
func (s *Store) WhiteNoiseDestination(connection, conversation string) bool {
	r := s.WhiteNoiseLink()
	if !r.Enabled || r.ResourceID != connection {
		return false
	}
	for _, c := range r.Conversations {
		if c.ID == conversation {
			for _, u := range r.Users {
				if u.PublicKey == c.Sender {
					return true
				}
			}
		}
	}
	return false
}

// Authorization, conversation binding, dedup, and enqueue share one disk commit.
func (s *Store) EnqueueWhiteNoise(m WhiteNoiseMail) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &s.state.WhiteNoise
	if !r.Enabled || r.ResourceID != m.ConnectionID {
		return errors.New("connection unavailable")
	}
	allowed := false
	for _, u := range r.Users {
		if u.PublicKey == m.SenderID {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("sender not authorized")
	}
	for _, id := range r.Seen {
		if id == m.ID {
			return nil
		}
	}
	if len(r.Mailbox) >= mailboxCap {
		return errors.New("White Noise mailbox full")
	}
	found := false
	for _, c := range r.Conversations {
		if c.ID == m.ConversationID {
			if c.Sender != m.SenderID {
				return errors.New("conversation sender changed")
			}
			found = true
		}
	}
	if !found {
		r.Conversations = append(r.Conversations, WhiteNoiseConversation{m.ConversationID, m.SenderID})
	}
	m.Channel = "whitenoise"
	m.State = MailPending
	m.Received = time.Now().UTC()
	r.Mailbox = append(r.Mailbox, m)
	r.Seen = append(r.Seen, m.ID)
	if len(r.Seen) > 10000 {
		r.Seen = r.Seen[len(r.Seen)-10000:]
	}
	err := s.save()
	if err == nil {
		s.signalMailboxLocked()
	}
	return err
}
func (s *Store) TakeWhiteNoise(worker string, now time.Time) ([]WhiteNoiseMail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &s.state.WhiteNoise
	if !r.Enabled || r.Supervisor.Worker != worker || !r.Supervisor.ExpiresAt.After(now) {
		return nil, errors.New("not the supervisor")
	}
	r.Supervisor.LastMailboxPoll = now
	out := []WhiteNoiseMail{}
	for i := range r.Mailbox {
		m := &r.Mailbox[i]
		if m.State == MailDelivered && m.DeliveredAt.Add(MailVisibility).Before(now) {
			m.State = MailPending
		}
		if m.State == MailPending && len(out) < 20 {
			m.State = MailDelivered
			m.DeliveredAt = now
			out = append(out, *m)
		}
	}
	if len(out) > 0 {
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (s *Store) AckWhiteNoise(worker string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &s.state.WhiteNoise
	if r.Supervisor.Worker != worker || !r.Supervisor.ExpiresAt.After(time.Now()) {
		return errors.New("not the supervisor")
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	kept := []WhiteNoiseMail{}
	for _, m := range r.Mailbox {
		if !want[m.ID] || m.State != MailDelivered {
			kept = append(kept, m)
		}
	}
	r.Mailbox = kept
	return s.save()
}
