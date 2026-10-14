// Package telegram is the gateway's Telegram front door.
//
// The gateway is the only holder of a bot token and the only thing that talks
// to Telegram. Workers never see the token and never speak to Telegram: they
// receive messages the gateway has already attributed to an allowed account.
//
// The Manager is a process-lifetime service, started once from the gateway
// binary the way technocore.Run is. It watches the stored token rather than
// being pushed a change, so connecting or disconnecting a bot is just a store
// write plus a Kick to cut the current long poll short.
package telegram

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"piso/gateway/internal/store"
)

// LongPoll is the getUpdates long-poll window. Telegram holds the request open
// until an update arrives or the window closes, which is what keeps an idle bot
// from spinning.
const LongPoll = 30 * time.Second

// longPollWindow bounds one request. It is longer than LongPoll so a slow
// response is not cut off, but a wedged connection still fails.
const longPollWindow = LongPoll + 15*time.Second

// ClaimTTL is how long an issued claim code stays usable.
const ClaimTTL = 10 * time.Minute

// claimAttempts is how many wrong codes invalidate a pending claim. The code is
// the only thing standing between a stranger who found the bot and the agent,
// so guessing is cut off rather than merely made unlikely.
const claimAttempts = 5

// claimAlphabet omits 0/O and 1/I so a code survives being read off a screen.
const claimAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

const claimLength = 8

// Status is the live view of the front door for the dashboard.
type Status struct {
	Running     bool                 `json:"running"`
	BotID       int64                `json:"botId,omitempty"`
	BotUsername string               `json:"botUsername,omitempty"`
	LastPoll    *time.Time           `json:"lastPoll,omitempty"`
	LastError   string               `json:"lastError,omitempty"`
	Claim       Claim                `json:"claim,omitempty"`
	Users       []store.TelegramUser `json:"users,omitempty"`
}

// Claim is a pending claim code. It is never persisted: a code exists to prove
// the sender controls the account right now, so anything on disk would be a
// replayable artifact with a longer life than it deserves. A restart drops it
// and the dashboard issues a new one.
type Claim struct {
	Code      string    `json:"code,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

type claimState struct {
	Code      string
	ExpiresAt time.Time
	Attempts  int
}

// Manager owns the bot connection: the long poll, the update cursor, the claim
// handshake and the allow-list. One gateway runs one Manager.
type Manager struct {
	Store *store.Store
	// HTTP is the outbound client. Nil means http.DefaultClient, which is the
	// production case: the gateway reaches Telegram directly.
	HTTP *http.Client
	// Handle delivers an authorized message. Nil means no worker is attached
	// yet, and the manager answers with an acknowledgement instead, so the
	// front door can be verified end to end before delivery is wired in.
	Handle func(ctx context.Context, m Message) error

	mu            sync.Mutex
	status        Status
	claim         claimState
	cancelSession context.CancelFunc
	now           func() time.Time
}

// NewManager returns a manager over st. Nothing runs until Run is called.
func NewManager(st *store.Store, h *http.Client) *Manager {
	return &Manager{Store: st, HTTP: h, now: time.Now}
}

func (m *Manager) clock() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

// token returns the stored bot token, or "" when no bot is connected.
func (m *Manager) token() string {
	rec, ok := Secret(m.Store.Secrets())
	if !ok {
		return ""
	}
	return rec.Value
}

// Kick cuts the current long poll short. Connecting or disconnecting a bot
// calls it so the change takes effect immediately instead of after the current
// 30s window closes.
func (m *Manager) Kick() {
	m.mu.Lock()
	cancel := m.cancelSession
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) setStatus(mut func(*Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mut(&m.status)
}

// Run is the process-lifetime loop. It re-reads the stored token every pass, so
// a connect or disconnect is picked up without any plumbing into the loop.
func (m *Manager) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		token := m.token()
		if token == "" {
			m.setStatus(func(s *Status) { *s = Status{} })
			if !sleep(ctx, 5*time.Second) {
				return
			}
			continue
		}
		err := m.session(ctx, token)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// Either the token changed or we were kicked: re-read and restart.
			backoff = time.Second
			continue
		}
		log.Printf("telegram: %v", err)
		m.setStatus(func(s *Status) {
			s.Running = false
			s.LastError = err.Error()
		})
		if !sleep(ctx, backoff) {
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// session polls until the token changes, the context ends, or Telegram fails.
func (m *Manager) session(ctx context.Context, token string) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.mu.Lock()
	m.cancelSession = cancel
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.cancelSession = nil
		m.mu.Unlock()
	}()

	c := NewClient(m.HTTP)
	bot, err := c.GetMe(sctx, token)
	if err != nil {
		return err
	}
	m.setStatus(func(s *Status) {
		s.Running = true
		s.BotID = bot.ID
		s.BotUsername = bot.Username
		s.LastError = ""
	})
	// A webhook on the bot makes getUpdates fail with 409. Taking the bot over
	// for polling means clearing it; a failure here is not fatal because the
	// 409 that follows is the real signal.
	if err := c.DeleteWebhook(sctx, token); err != nil {
		log.Printf("telegram: deleteWebhook: %v", err)
	}

	offset := m.Store.TelegramLink().Offset
	for {
		if sctx.Err() != nil {
			return nil
		}
		if m.token() != token {
			return nil
		}
		uctx, ucancel := context.WithTimeout(sctx, longPollWindow)
		updates, err := c.GetUpdates(uctx, token, offset, LongPoll)
		ucancel()
		if err != nil {
			if sctx.Err() != nil {
				return nil
			}
			return err
		}
		for i := range updates {
			m.deliver(sctx, c, token, updates[i])
		}
		if n := len(updates); n > 0 {
			// Updates arrive in ascending order, so the last one plus one
			// acknowledges the whole batch. Delivery above is at-least-once
			// across a crash; de-duplicating a redelivered message belongs to
			// whoever consumes it.
			offset = updates[n-1].UpdateID + 1
			if err := m.Store.SetTelegramOffset(offset); err != nil {
				log.Printf("telegram: offset: %v", err)
			}
		}
		m.setStatus(func(s *Status) {
			now := m.clock()
			s.LastPoll = &now
		})
	}
}

// deliver routes one update. It never returns an error: the cursor must keep
// moving, because an update left unacknowledged is redelivered forever and
// turns the long poll into a busy loop.
func (m *Manager) deliver(ctx context.Context, c *Client, token string, u Update) {
	msg := u.Message
	if msg == nil || msg.From == nil || msg.From.IsBot {
		return
	}
	// Only private chats. In a group the chat id is the group and From is
	// merely whoever spoke, so binding a group would let any member act as the
	// owner.
	if msg.Chat.Type != "private" {
		return
	}
	text := strings.TrimSpace(msg.Text)
	userID := msg.From.ID

	if code, ok := claimCode(text); ok {
		if _, allowed := m.Store.TelegramAllowed(userID); allowed {
			m.reply(ctx, c, token, msg.Chat.ID, "This chat is already connected.")
			return
		}
		m.redeem(ctx, c, token, msg, code)
		return
	}
	if _, allowed := m.Store.TelegramAllowed(userID); !allowed {
		// /start is what a first-time visitor actually sends, so it gets the
		// instructions. Anything else from a stranger is ignored: answering
		// every unknown sender would make the bot a megaphone.
		if text == "/start" || strings.HasPrefix(text, "/start ") {
			m.reply(ctx, c, token, msg.Chat.ID, "This bot is private. Open the Piso dashboard, choose Access, then Telegram, and send the claim code it shows.")
		}
		return
	}
	m.refresh(ctx, userID, msg.From)
	if m.Handle == nil {
		m.reply(ctx, c, token, msg.Chat.ID, "Received. No worker is attached to this gateway yet, so nothing was run.")
		return
	}
	if err := m.Handle(ctx, *msg); err != nil {
		log.Printf("telegram: handle: %v", err)
		m.reply(ctx, c, token, msg.Chat.ID, "That message could not be delivered to the worker.")
	}
}

// redeem consumes a claim code. The code is the only thing between a stranger
// who found the bot and the agent, so the comparison is constant time and a
// short run of wrong guesses burns the code.
func (m *Manager) redeem(ctx context.Context, c *Client, token string, msg *Message, code string) {
	now := m.clock()
	m.mu.Lock()
	pending := m.claim
	live := pending.Code != "" && now.Before(pending.ExpiresAt)
	match := live && subtle.ConstantTimeCompare([]byte(code), []byte(pending.Code)) == 1
	switch {
	case match:
		m.claim = claimState{}
	case live:
		pending.Attempts++
		if pending.Attempts >= claimAttempts {
			m.claim = claimState{}
		} else {
			m.claim = pending
		}
	}
	m.mu.Unlock()
	if !match {
		m.reply(ctx, c, token, msg.Chat.ID, "That claim code is not valid. Open the Piso dashboard and copy the current one.")
		return
	}
	user := store.TelegramUser{
		UserID:    msg.From.ID,
		ChatID:    msg.Chat.ID,
		Username:  msg.From.Username,
		FirstName: msg.From.FirstName,
		ClaimedAt: now,
	}
	if err := m.Store.UpsertTelegramUser(user); err != nil {
		log.Printf("telegram: claim: %v", err)
		m.reply(ctx, c, token, msg.Chat.ID, "The claim could not be saved. Try again.")
		return
	}
	log.Printf("telegram: chat %d claimed", msg.From.ID)
	m.reply(ctx, c, token, msg.Chat.ID, "Connected. Messages from this chat now reach the gateway.")
}

// refresh keeps the stored name current. Usernames change, and the dashboard
// should show what the account calls itself now.
func (m *Manager) refresh(ctx context.Context, userID int64, from *User) {
	known, ok := m.Store.TelegramAllowed(userID)
	if !ok || (known.Username == from.Username && known.FirstName == from.FirstName) {
		return
	}
	known.Username, known.FirstName = from.Username, from.FirstName
	if err := m.Store.UpsertTelegramUser(known); err != nil {
		log.Printf("telegram: refresh: %v", err)
	}
}

func (m *Manager) reply(ctx context.Context, c *Client, token string, chatID int64, text string) {
	if err := c.SendMessage(ctx, token, chatID, text); err != nil {
		log.Printf("telegram: reply: %v", err)
	}
}

// IssueClaim mints a claim code. One code is outstanding at a time: issuing a
// new one invalidates the previous, so the dashboard only ever shows one.
func (m *Manager) IssueClaim() (Claim, error) {
	if m.token() == "" {
		return Claim{}, errors.New("no bot token is stored")
	}
	code, err := randomCode()
	if err != nil {
		return Claim{}, err
	}
	out := Claim{Code: code, ExpiresAt: m.clock().Add(ClaimTTL)}
	m.mu.Lock()
	m.claim = claimState{Code: code, ExpiresAt: out.ExpiresAt}
	m.mu.Unlock()
	return out, nil
}

// Snapshot returns the live status plus the persisted link. The link is read
// from the store rather than cached here, so the dashboard sees one source of
// truth for the cursor and the allow-list.
func (m *Manager) Snapshot() Status {
	now := m.clock()
	m.mu.Lock()
	out := m.status
	// The pending code lives in the claim state, which is the only thing that
	// knows whether it is still live. An expired code is not shown at all.
	if m.claim.Code != "" && now.Before(m.claim.ExpiresAt) {
		out.Claim = Claim{Code: m.claim.Code, ExpiresAt: m.claim.ExpiresAt}
	} else {
		out.Claim = Claim{}
	}
	m.mu.Unlock()
	link := m.Store.TelegramLink()
	out.Users = link.Users
	if out.BotID == 0 {
		// A restart has a stored bot id before the first poll completes.
		out.BotID = link.BotID
	}
	return out
}

// Revoke removes an account from the allow-list.
func (m *Manager) Revoke(userID int64) (bool, error) {
	return m.Store.RemoveTelegramUser(userID)
}

// claimCode parses "/claim <code>" and returns the canonical code: uppercase,
// with any separator removed. The alphabet has no dashes, so the grouping the
// dashboard shows is presentation only.
func claimCode(text string) (string, bool) {
	fields := strings.Fields(text)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "/claim") {
		return "", false
	}
	code := strings.ToUpper(strings.ReplaceAll(fields[1], "-", ""))
	if len(code) != claimLength {
		return "", false
	}
	for _, r := range code {
		if !strings.ContainsRune(claimAlphabet, r) {
			return "", false
		}
	}
	return code, true
}

// randomCode returns a claim code: claimLength characters from an unambiguous
// alphabet, with no separator. The dashboard groups it for display and
// claimCode strips whatever separator the sender types.
//
// The alphabet is 32 characters and a byte has 256 values, so the modulo is
// exact: every character is equally likely.
func randomCode() (string, error) {
	raw := make([]byte, claimLength)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, v := range raw {
		b.WriteByte(claimAlphabet[int(v)%len(claimAlphabet)])
	}
	return b.String(), nil
}

// sleep waits for d, reporting false if the context ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
