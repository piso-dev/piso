package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"piso/gateway/internal/store"
	"piso/gateway/internal/telegram"
)

// telegramMessageMax is Telegram's own cap on a single message body. A worker
// reply longer than this is truncated rather than rejected, so an over-long
// answer still reaches the chat.
const telegramMessageMax = 4096

// messageWaitMax bounds one long-poll. The worker reconnects immediately after
// an empty or timed-out poll, so a short window still feels instant while
// keeping any intermediary from holding a connection too long.
const messageWaitMax = 30

// attachTelegramMailbox routes an authorized Telegram message into the gateway
// mailbox for the live supervisor. With no supervisor the poller keeps its
// no-worker acknowledgement, so a message is never silently accepted and lost.
func (s *Server) attachTelegramMailbox() {
	if s.Telegram == nil {
		return
	}
	s.Telegram.Handle = func(_ context.Context, m telegram.Message) error {
		if !s.hasLiveSupervisor() {
			return telegram.ErrNoRecipient
		}
		text := strings.TrimSpace(m.Text)
		if text == "" {
			return nil
		}
		_, err := s.Store.EnqueueMail(store.MailMessage{
			ChatID: m.Chat.ID,
			UserID: m.From.ID,
			Text:   text,
		})
		return err
	}
}

func (s *Server) hasLiveSupervisor() bool {
	_, live := s.Store.Supervisor(time.Now())
	return live
}

// liveSupervisor reports whether worker is the one the coordinator named. The
// mailbox belongs to the bot, not to a worker, so a handoff is automatic: the
// next supervisor drains whatever the previous one left behind.
func (s *Server) liveSupervisor(worker string) bool {
	sup, live := s.Store.Supervisor(time.Now())
	return live && sup.Worker == worker
}

// workerIdentityOK applies the same slug<->IP registry check as every other
// worker route: a worker may only act as itself. A 403 writes the response.
func (s *Server) workerIdentityOK(w http.ResponseWriter, r *http.Request, worker string) bool {
	if reg, ok := s.Store.WorkerByIP(requestIP(r)); ok && reg.Name != worker {
		identityMismatch(w, r, reg, worker, "")
		return false
	}
	return true
}

// handleWorkerGetMessages is the supervisor's long-poll. It hands out pending
// Telegram messages, waiting up to `wait` seconds if the queue is empty. A
// hand-out is not a removal: the worker acks after it has them, so a crash
// mid-turn redelivers rather than drops.
func (s *Server) handleWorkerGetMessages(w http.ResponseWriter, r *http.Request) {
	worker := strings.TrimSpace(r.URL.Query().Get("worker"))
	if !workerNameRe.MatchString(worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	if !s.workerIdentityOK(w, r, worker) {
		return
	}
	// Only the live supervisor may drain the mailbox. A worker that lost the
	// lease gets a 409 and stops polling, so a handoff cannot deliver to two.
	if !s.liveSupervisor(worker) {
		writeJSON(w, 409, map[string]string{"error": "not the supervisor"})
		return
	}
	wait := atoiDefault(r.URL.Query().Get("wait"), 25)
	if wait > messageWaitMax {
		wait = messageWaitMax
	}
	if wait < 1 {
		wait = 1
	}
	deadline := time.NewTimer(time.Duration(wait) * time.Second)
	defer deadline.Stop()
	for {
		if msgs := s.Store.TakeMail(time.Now(), 20); len(msgs) > 0 {
			writeJSON(w, 200, map[string]any{"messages": msgs})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			writeJSON(w, 200, map[string]any{"messages": []store.MailMessage{}})
			return
		case <-s.Store.SubMailbox():
		}
	}
}

// handleWorkerAckMessages removes messages the worker has taken. Acking is
// allowed even after the lease has moved: the ids are opaque and the message
// must not come back just because a handoff happened mid-turn.
func (s *Server) handleWorkerAckMessages(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Worker string   `json:"worker"`
		IDs    []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Worker = strings.TrimSpace(in.Worker)
	if !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	if !s.workerIdentityOK(w, r, in.Worker) {
		return
	}
	if err := s.Store.AckMail(in.IDs); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "acked": len(in.IDs)})
}

// handleWorkerReply posts a supervisor's answer to the chat. The worker never
// holds the bot token, so it hands the text to the gateway; the gateway only
// sends to an account that is already in the allow-list, so a compromised or
// buggy worker cannot use the bot as a megaphone.
func (s *Server) handleWorkerReply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Worker string `json:"worker"`
		ChatID int64  `json:"chatId"`
		Text   string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Worker = strings.TrimSpace(in.Worker)
	in.Text = strings.TrimSpace(in.Text)
	if !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	if in.Text == "" {
		writeJSON(w, 400, map[string]string{"error": "text required"})
		return
	}
	if !s.workerIdentityOK(w, r, in.Worker) {
		return
	}
	if !s.liveSupervisor(in.Worker) {
		writeJSON(w, 409, map[string]string{"error": "not the supervisor"})
		return
	}
	if _, ok := s.Store.TelegramAllowed(in.ChatID); !ok {
		writeJSON(w, 403, map[string]string{"error": "chat is not in the allow-list"})
		return
	}
	if s.Telegram == nil {
		writeJSON(w, 409, map[string]string{"error": "no telegram bot is connected"})
		return
	}
	if len(in.Text) > telegramMessageMax {
		in.Text = in.Text[:telegramMessageMax]
	}
	if err := s.Telegram.SendText(r.Context(), in.ChatID, in.Text); err != nil {
		writeJSON(w, 502, map[string]string{"error": "telegram is unreachable right now"})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
