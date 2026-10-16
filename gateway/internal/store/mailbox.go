package store

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// MailState is where a queued Telegram message is in the delivery handshake.
type MailState string

const (
	// MailPending is accepted but not yet handed to the worker.
	MailPending MailState = "pending"
	// MailDelivered is handed out and awaiting an ack. If no ack arrives before
	// the visibility window it goes back to pending and is redelivered.
	MailDelivered MailState = "delivered"
)

// MailMessage is one authorized Telegram message waiting for the supervisor.
// It is the durable queue between the poller (which owns the Telegram cursor)
// and the worker (which may be absent, restarting, or busy).
type MailMessage struct {
	ID       string    `json:"id"`
	ChatID   int64     `json:"chatId"`
	UserID   int64     `json:"userId,omitempty"`
	Text     string    `json:"text"`
	State    MailState `json:"state"`
	Received time.Time `json:"receivedAt"`
	// DeliveredAt bounds the at-least-once redelivery window.
	DeliveredAt time.Time `json:"deliveredAt,omitempty"`
	// Attempts counts redeliveries, so a message that keeps failing is visible.
	Attempts int `json:"attempts,omitempty"`
}

const (
	// mailboxCap bounds the queue. A dead worker must not grow state.json
	// without limit; the oldest pending message is dropped on overflow.
	mailboxCap = 100
	// MailVisibility is how long a handed-out message waits for an ack before
	// it is redelivered. Delivery is at-least-once, never silently dropped.
	MailVisibility = 120 * time.Second
)

func newMailID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

// EnqueueMail appends an authorized message for the supervisor and wakes any
// long-poll. The queue is bounded; on overflow the oldest message is dropped.
func (s *Store) EnqueueMail(m MailMessage) (MailMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		m.ID = newMailID()
	}
	if m.Received.IsZero() {
		m.Received = time.Now().UTC()
	}
	m.State = MailPending
	m.DeliveredAt = time.Time{}
	s.state.Mailbox = append(s.state.Mailbox, m)
	if len(s.state.Mailbox) > mailboxCap {
		s.state.Mailbox = s.state.Mailbox[len(s.state.Mailbox)-mailboxCap:]
	}
	if err := s.save(); err != nil {
		return MailMessage{}, err
	}
	s.signalMailboxLocked()
	return m, nil
}

// TakeMail requeues delivered messages whose visibility window has passed, then
// hands out up to limit pending messages and marks them delivered. A hand-out
// is not a removal: only AckMail removes, so a worker that dies mid-turn gets
// the message again.
func (s *Store) TakeMail(now time.Time, limit int) []MailMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 1 {
		limit = 1
	}
	for i := range s.state.Mailbox {
		m := &s.state.Mailbox[i]
		if m.State == MailDelivered && !m.DeliveredAt.IsZero() && now.Sub(m.DeliveredAt) >= MailVisibility {
			m.State = MailPending
			m.DeliveredAt = time.Time{}
			m.Attempts++
		}
	}
	out := make([]MailMessage, 0, limit)
	for i := range s.state.Mailbox {
		if len(out) >= limit {
			break
		}
		m := &s.state.Mailbox[i]
		if m.State != MailPending {
			continue
		}
		m.State = MailDelivered
		m.DeliveredAt = now
		out = append(out, *m)
	}
	if len(out) > 0 {
		_ = s.save()
	}
	return out
}

// AckMail removes messages by id. Unknown ids are ignored, so a redelivered
// message acked twice is harmless.
func (s *Store) AckMail(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]MailMessage, 0, len(s.state.Mailbox))
	for _, m := range s.state.Mailbox {
		if want[m.ID] {
			continue
		}
		kept = append(kept, m)
	}
	s.state.Mailbox = kept
	return s.save()
}

// Mail returns a copy of the mailbox, for the dashboard.
func (s *Store) Mail() []MailMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]MailMessage(nil), s.state.Mailbox...)
}

// PendingMail reports whether anything is waiting, so a poll loop can skip
// arming a wait when there is already work.
func (s *Store) PendingMail() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.state.Mailbox {
		if m.State == MailPending {
			return true
		}
	}
	return false
}

// SubMailbox returns the wake channel for new mail. Callers must drain it.
func (s *Store) SubMailbox() <-chan struct{} { return s.onMailbox }

// signalMailboxLocked wakes long-polls. Like asks, a pending signal is enough:
// several enqueues before a poller wakes still mean one mailbox read.
func (s *Store) signalMailboxLocked() {
	select {
	case s.onMailbox <- struct{}{}:
	default:
	}
}
