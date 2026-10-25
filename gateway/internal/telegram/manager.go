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
//
// The bot token is replicated to every enrolled gateway, but Telegram allows
// only one getUpdates at a time. The Manager therefore holds a lease granted by
// the coordinator (see the Lease field): every gateway asks, exactly one is
// granted, and the holder must keep renewing or the lease lapses and another
// gateway takes over.
package telegram

import (
	"context"
	"crypto/rand"
	"errors"
	"log"
	mrand "math/rand"
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

// LeaseTTL is the poller-lease window the gateway asks the coordinator for. It
// is longer than one long poll so a request already in flight cannot outlive
// the lease, and the holder renews well inside it.
const LeaseTTL = 120 * time.Second

// leaseRenew is how often the holder re-asks. A third of the TTL leaves room
// for a lost round trip before the lease actually lapses.
const leaseRenew = LeaseTTL / 3

// leaseRetry is how long a gateway waits before asking again after a refusal,
// jittered so a fleet that lost a holder does not stampede the coordinator.
const leaseRetry = 15 * time.Second

// errLeaseLost ends a polling session when the coordinator stops granting this
// gateway the lease: another gateway holds it now, so continuing to poll would
// only make the two terminate each other's getUpdates.
var errLeaseLost = errors.New("telegram poller lease lost")

// ErrNoRecipient is what Handle returns when no live supervisor can receive the
// message. The poller turns it into the no-worker acknowledgement rather than
// the generic delivery failure, so the two are distinguishable.
var ErrNoRecipient = errors.New("no supervisor is attached to this gateway")

// noWorkerMessage is the acknowledgement when the front door works but no
// supervisor is attached. It is deliberately the same whether Handle is nil or
// returns ErrNoRecipient.
const noWorkerMessage = "Received. No worker is attached to this gateway yet, so nothing was run."

// ClaimTTL is how long an issued claim code stays usable.
const ClaimTTL = 10 * time.Minute

// claimAttempts is how many wrong codes invalidate a pending claim. The code is
// the only thing standing between a stranger who found the bot and the agent,
// so guessing is cut off rather than merely made unlikely.
const claimAttempts = 5

// claimAlphabet omits 0/O and 1/I so a code survives being read off a screen.
const claimAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

const claimLength = 8

// Status is the live view of the poller for the dashboard.
type Status struct {
	Running     bool       `json:"running"`
	BotID       int64      `json:"botId,omitempty"`
	BotUsername string     `json:"botUsername,omitempty"`
	LastPoll    *time.Time `json:"lastPoll,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
	// LeaseHeld is true while this gateway holds the poller lease; Waiting is
	// true while it is asking and another gateway holds it. Both are false when
	// no lease is in use (a standalone gateway with no coordinator to ask).
	LeaseHeld bool `json:"leaseHeld,omitempty"`
	Waiting   bool `json:"waiting,omitempty"`
}

// LeaseGrant is the coordinator's answer to a poller-lease request: Granted is
// false when another gateway holds a live lease, Offset is the coordinator's
// last known update cursor, and ExpiresAt bounds how long this gateway may keep
// polling without another successful renewal.
type LeaseGrant struct {
	Granted   bool
	Offset    int64
	ExpiresAt time.Time
	Holder    string
}

// Manager owns the bot connection: the long poll, the update cursor and the
// lease. Who may talk to the bot belongs to the front door, which is the
// coordinator once the token is published and the local store otherwise.
type Manager struct {
	Store *store.Store
	// HTTP is the outbound client. Nil means http.DefaultClient, which is the
	// production case: the gateway reaches Telegram directly.
	HTTP *http.Client
	// Handle delivers an authorized message. Nil means no worker is attached
	// yet, and the manager answers with an acknowledgement instead, so the
	// front door can be verified end to end before delivery is wired in.
	Handle func(ctx context.Context, m Message) error
	// Lease asks the coordinator for the single-poller lease on a bot resource.
	// Nil means this gateway polls without one: a standalone gateway has no
	// coordinator to ask, and a test drives the poller directly. A bot that was
	// never published (no coordinator resource id) is polled directly too,
	// because there is nothing to arbitrate.
	Lease func(ctx context.Context, resourceID string, offset int64) (LeaseGrant, error)
	// Local is the store-backed front door: the authority for a gateway with no
	// coordinator, and the no-network fallback for the dashboard.
	Local FrontDoor
	// Remote is the coordinator-backed front door. Nil means this gateway has
	// no coordinator; the server sets it once the pairing is known.
	Remote FrontDoor

	mu            sync.Mutex
	status        Status
	cancelSession context.CancelFunc
	now           func() time.Time
}

// NewManager returns a manager over st. Nothing runs until Run is called.
func NewManager(st *store.Store, h *http.Client) *Manager {
	m := &Manager{Store: st, HTTP: h, now: time.Now}
	// The local front door reads this manager's clock, so a test that swaps now
	// still controls claim expiry.
	m.Local = NewLocalFrontDoor(st, m.clock)
	return m
}

func (m *Manager) clock() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

// token returns the stored bot token, or "" when no bot is connected.
func (m *Manager) token() string {
	rec, ok := m.bot()
	if !ok {
		return ""
	}
	return rec.Value
}

// bot returns the stored bot-token secret, or ok=false when no bot is
// connected. The whole record is returned, not just the value: the poller lease
// is keyed by the coordinator resource id it carries.
func (m *Manager) bot() (store.SecretRec, bool) {
	return Secret(m.Store.Secrets())
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

// setWaiting records that the front door is serving nobody because another
// gateway holds the poller lease. That is a normal state, not an error.
func (m *Manager) setWaiting() {
	m.setStatus(func(s *Status) {
		s.Running = false
		s.LeaseHeld = false
		s.Waiting = true
		s.LastError = ""
	})
}

// setFailed records a real failure: the bot cannot be served and no other
// gateway is known to be serving it either.
func (m *Manager) setFailed(err error) {
	m.setStatus(func(s *Status) {
		s.Running = false
		s.LeaseHeld = false
		s.Waiting = false
		s.LastError = err.Error()
	})
}

// Run is the process-lifetime loop. It re-reads the stored token every pass, so
// a connect or disconnect is picked up without any plumbing into the loop, and
// it re-asks the coordinator for the poller lease on every pass: a gateway that
// is refused keeps asking, and a holder that lapses is replaced.
func (m *Manager) Run(ctx context.Context) {
	go m.watchFrontDoor(ctx)
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		rec, ok := m.bot()
		if !ok {
			m.setStatus(func(s *Status) { *s = Status{} })
			if !sleep(ctx, 5*time.Second) {
				return
			}
			continue
		}
		grant, held, err := m.acquire(ctx, rec)
		if err != nil {
			// The coordinator is the only arbiter, so without an answer this
			// gateway must not poll: another gateway may be polling already.
			log.Printf("telegram: lease: %v", err)
			m.setFailed(err)
			if !sleep(ctx, jitter(backoff)) {
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		if !held {
			m.setWaiting()
			if !sleep(ctx, jitter(leaseRetry)) {
				return
			}
			continue
		}
		// The allow-list is the coordinator's, so fetch it before serving: a
		// gateway taking over the lease must not enforce the list it inherited.
		if err := m.Refresh(); err != nil {
			// Fall back to the cache. An empty list means deny, never allow.
			log.Printf("telegram: front door: %v", err)
		}
		err = m.session(ctx, rec, grant)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// Either the token changed or we were kicked: re-read and restart.
			backoff = time.Second
			continue
		}
		if errors.Is(err, errLeaseLost) {
			// Another gateway holds the lease now. Re-ask rather than report an
			// error the dashboard cannot act on.
			m.setWaiting()
			if !sleep(ctx, jitter(leaseRetry)) {
				return
			}
			continue
		}
		log.Printf("telegram: %v", err)
		m.setFailed(err)
		if !sleep(ctx, jitter(backoff)) {
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// watchFrontDoor refetches the allow-list whenever the coordinator says the
// front door moved. Without it a revocation would not take effect until the
// next lease renewal, and the lease only renews every 40s.
func (m *Manager) watchFrontDoor(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.Store.SubTelegram():
			_ = m.Refresh()
		}
	}
}

// acquire asks the coordinator for the lease, or renews it when this gateway
// already holds it. held is false when another gateway holds a live lease.
//
// With no lease client, or a bot that was never published to the coordinator,
// there is nothing to arbitrate, so an empty grant is returned and polling
// proceeds directly.
func (m *Manager) acquire(ctx context.Context, rec store.SecretRec) (LeaseGrant, bool, error) {
	if m.Lease == nil || rec.TechnocoreID == "" {
		return LeaseGrant{}, true, nil
	}
	grant, err := m.Lease(ctx, rec.TechnocoreID, m.Store.TelegramLink().Offset)
	if err != nil {
		return LeaseGrant{}, false, err
	}
	return grant, grant.Granted, nil
}

// session polls until the token changes, the context ends, the lease lapses, or
// Telegram fails.
func (m *Manager) session(ctx context.Context, rec store.SecretRec, grant LeaseGrant) error {
	token := rec.Value
	managed := m.Lease != nil && rec.TechnocoreID != ""
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
		s.LeaseHeld = managed
		s.Waiting = false
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

	// The coordinator's cursor travels with the lease, so a gateway taking over
	// resumes where the previous holder stopped. Never move it backwards: the
	// local copy may be ahead of the last reported one.
	offset := m.Store.TelegramLink().Offset
	if grant.Offset > offset {
		offset = grant.Offset
	}
	expiresAt := grant.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = m.clock().Add(LeaseTTL)
	}
	renewAt := m.clock().Add(leaseRenew)

	for {
		if sctx.Err() != nil {
			return nil
		}
		cur, ok := m.bot()
		if !ok || cur.Value != token {
			return nil
		}
		if m.Lease != nil && !managed && cur.TechnocoreID != "" {
			// The bot was published and this gateway now has a coordinator to
			// lease from, so restart to acquire one: peers can receive the token,
			// and one of them may already be about to poll.
			return nil
		}
		if managed && !m.clock().Before(renewAt) {
			next, err := m.Lease(sctx, rec.TechnocoreID, offset)
			switch {
			case err == nil && !next.Granted:
				return errLeaseLost
			case err == nil:
				renewAt = m.clock().Add(leaseRenew)
				if next.ExpiresAt.IsZero() {
					expiresAt = m.clock().Add(LeaseTTL)
				} else {
					expiresAt = next.ExpiresAt
				}
			case m.clock().After(expiresAt):
				// The round trip failed and the window it was renewing has
				// closed: this gateway can no longer claim the bot.
				return errLeaseLost
			default:
				// A lost round trip is not a lost lease. Retry before it lapses.
				renewAt = m.clock().Add(leaseRenew)
			}
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
		m.reply(ctx, c, token, msg.Chat.ID, noWorkerMessage)
		return
	}
	if err := m.Handle(ctx, *msg); err != nil {
		if errors.Is(err, ErrNoRecipient) {
			m.reply(ctx, c, token, msg.Chat.ID, noWorkerMessage)
			return
		}
		log.Printf("telegram: handle: %v", err)
		m.reply(ctx, c, token, msg.Chat.ID, "That message could not be delivered to the worker.")
	}
}

// redeem forwards a claim code to the front door. The code is the only thing
// between a stranger who found the bot and the agent, so whoever owns it — the
// coordinator once the bot is published, the local front door otherwise —
// compares it in constant time and burns it after a few wrong guesses.
func (m *Manager) redeem(ctx context.Context, c *Client, token string, msg *Message, code string) {
	// The front door owns the code and the attempt counter. Once the bot is
	// published that is the coordinator, which is what lets a code issued from
	// any dashboard be redeemed through whichever gateway holds the lease.
	user := store.TelegramUser{
		UserID:    msg.From.ID,
		ChatID:    msg.Chat.ID,
		Username:  msg.From.Username,
		FirstName: msg.From.FirstName,
	}
	allowed, _, err := m.frontDoor().Redeem(code, user)
	if err != nil {
		log.Printf("telegram: claim: %v", err)
		m.reply(ctx, c, token, msg.Chat.ID, "The claim could not be checked. Try again.")
		return
	}
	if !allowed {
		m.reply(ctx, c, token, msg.Chat.ID, "That claim code is not valid. Open the Piso dashboard and copy the current one.")
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
	if err := m.frontDoor().Upsert(known); err != nil {
		log.Printf("telegram: refresh: %v", err)
	}
}

func (m *Manager) reply(ctx context.Context, c *Client, token string, chatID int64, text string) {
	if err := c.SendMessage(ctx, token, chatID, text); err != nil {
		log.Printf("telegram: reply: %v", err)
	}
}

// SendText posts a message to a chat. It is how a worker's reply reaches
// Telegram: the worker never holds the token, so it hands the text to the
// gateway and the gateway sends it. Callers restrict the chat to the
// allow-list; this only enforces that a token exists.
func (m *Manager) SendText(ctx context.Context, chatID int64, text string) error {
	rec, ok := m.bot()
	if !ok {
		return errors.New("no telegram bot token is stored")
	}
	return NewClient(m.HTTP).SendMessage(ctx, rec.Value, chatID, text)
}

// SendStatus writes one chat's live activity line and returns the id to edit on
// the next call. A status line follows a turn rather than narrating it: the
// first call sends, every later call edits the same message, so a tool-heavy
// turn does not flood the chat with one message per step. messageID 0 sends a
// fresh message; an edit that Telegram refuses (the message was deleted, or it
// is too old to edit) falls back to sending, so the line can never wedge.
func (m *Manager) SendStatus(ctx context.Context, chatID int64, text string, messageID int64) (int64, error) {
	rec, ok := m.bot()
	if !ok {
		return 0, errors.New("no telegram bot token is stored")
	}
	c := NewClient(m.HTTP)
	if messageID > 0 {
		err := c.EditMessageText(ctx, rec.Value, chatID, messageID, text)
		if err == nil {
			return messageID, nil
		}
		// Telegram rejects an edit that would not change the text. That is the
		// normal outcome of a throttled flush, not a failure worth a new message.
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == 400 && strings.Contains(apiErr.Description, "not modified") {
			return messageID, nil
		}
	}
	return c.SendMessageID(ctx, rec.Value, chatID, text)
}

// IssueClaim mints a claim code. One code is outstanding at a time: issuing a
// new one invalidates the previous, so the dashboard only ever shows one. Once
// the bot is published the coordinator owns the code, so any gateway's
// dashboard can issue one the holder will accept.
func (m *Manager) IssueClaim() (Claim, error) {
	if m.token() == "" {
		return Claim{}, errors.New("no bot token is stored")
	}
	return m.frontDoor().Issue()
}

// Snapshot returns the live poller view. The front-door state (allow-list and
// pending claim) is separate: it can be a network call, and a caller that only
// needs liveness must not pay for one.
func (m *Manager) Snapshot() Status {
	m.mu.Lock()
	out := m.status
	m.mu.Unlock()
	if out.BotID == 0 {
		// A restart has a stored bot id before the first poll completes.
		out.BotID = m.Store.TelegramLink().BotID
	}
	return out
}

// FrontDoorState returns the allow-list and any pending claim. Once the bot is
// published this asks the coordinator, so every gateway's dashboard shows the
// same list; it is bounded by the coordinator client's timeout.
func (m *Manager) FrontDoorState() (FrontDoorState, error) {
	return m.frontDoor().State()
}

// LocalFrontDoorState is the no-network view, for when the coordinator cannot
// be reached: the cached allow-list and whatever claim this gateway holds.
func (m *Manager) LocalFrontDoorState() FrontDoorState {
	state, _ := m.Local.State()
	return state
}

// frontDoor is the authority for the bot's allow-list: the coordinator once the
// token is published to one, and the local store otherwise.
func (m *Manager) frontDoor() FrontDoor {
	if m.Remote != nil {
		if rec, ok := m.bot(); ok && rec.TechnocoreID != "" {
			return m.Remote
		}
	}
	return m.Local
}

// Refresh pulls the authoritative allow-list into the cache. The coordinator
// pushes after every membership change, and the poller calls it on taking the
// lease, so a holder never enforces a list the coordinator has moved past.
func (m *Manager) Refresh() error {
	if err := m.frontDoor().Refresh(); err != nil {
		log.Printf("telegram: front door: %v", err)
		return err
	}
	return nil
}

// Revoke removes an account from the allow-list.
func (m *Manager) Revoke(userID int64) (bool, error) {
	return m.frontDoor().Revoke(userID)
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

// jitter returns d ± 25%. Every gateway that is refused the lease waits, and
// without jitter they would all ask again on the same tick.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	span := int64(d) / 4
	if span <= 0 {
		return d
	}
	return d + time.Duration(mrand.Int63n(span*2+1)-span)
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
