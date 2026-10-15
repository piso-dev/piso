package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(
		filepath.Join(dir, "state.json"),
		filepath.Join(dir, "log.jsonl"),
		filepath.Join(dir, "patterns.json"),
		filepath.Join(dir, "activities.db"),
		20,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func addToken(t *testing.T, st *store.Store, token string) {
	t.Helper()
	if err := st.AddSecret(store.SecretRec{
		ID: "sec_test", Name: "Telegram bot", Kind: model.SecretKindTelegram,
		GatewayOnly: true, Value: token,
	}); err != nil {
		t.Fatal(err)
	}
}

// ---- a scripted Bot API -----------------------------------------------------

type sentMessage struct{ chat, text string }

// fakeBot answers the Bot API in process. getUpdates hands out whatever the
// test pushes on batches and blocks once the queue is empty, so the poll loop
// never spins and the test decides exactly when it advances.
type fakeBot struct {
	id       int64
	username string

	batches chan []Update

	mu       sync.Mutex
	sent     []sentMessage
	offsets  []string
	meCalls  int
	hookCall int
	fail     bool
}

func newFakeBot(id int64, username string) *fakeBot {
	return &fakeBot{id: id, username: username, batches: make(chan []Update, 16)}
}

func (f *fakeBot) push(updates ...Update) { f.batches <- updates }

func (f *fakeBot) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		form := url.Values{}
		if req.Body != nil {
			raw, _ := io.ReadAll(req.Body)
			form, _ = url.ParseQuery(string(raw))
		}
		switch method {
		case "getMe":
			f.mu.Lock()
			f.meCalls++
			f.mu.Unlock()
			return jsonResponse(map[string]any{"ok": true, "result": map[string]any{
				"id": f.id, "is_bot": true, "first_name": "Piso", "username": f.username,
			}}), nil
		case "deleteWebhook":
			f.mu.Lock()
			f.hookCall++
			f.mu.Unlock()
			return jsonResponse(map[string]any{"ok": true, "result": true}), nil
		case "sendMessage":
			f.mu.Lock()
			f.sent = append(f.sent, sentMessage{chat: form.Get("chat_id"), text: form.Get("text")})
			f.mu.Unlock()
			return jsonResponse(map[string]any{"ok": true, "result": map[string]any{}}), nil
		case "getUpdates":
			f.mu.Lock()
			f.offsets = append(f.offsets, form.Get("offset"))
			fail := f.fail
			f.mu.Unlock()
			if fail {
				return jsonResponse(map[string]any{"ok": false, "error_code": 500, "description": "Internal Server Error"}), nil
			}
			select {
			case batch := <-f.batches:
				return jsonResponse(map[string]any{"ok": true, "result": batch}), nil
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return nil, fmt.Errorf("unexpected method %s", method)
	})}
}

func (f *fakeBot) replies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.sent))
	for _, s := range f.sent {
		out = append(out, s.text)
	}
	return out
}

func (f *fakeBot) askedOffsets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.offsets...)
}

func jsonResponse(v any) *http.Response {
	raw, _ := json.Marshal(v)
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
}

// ---- fixtures ---------------------------------------------------------------

func privateMessage(updateID, userID int64, text string) Update {
	return Update{UpdateID: updateID, Message: &Message{
		MessageID: updateID, Text: text,
		Chat: Chat{ID: userID, Type: "private"},
		From: &User{ID: userID, FirstName: "Ada", Username: "ada"},
	}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// run starts the poller and stops it when the test ends.
func run(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// pendingClaim reads the live claim from the front door, which is where it
// lives: in memory locally, on the coordinator once the bot is published.
func pendingClaim(t *testing.T, m *Manager) Claim {
	t.Helper()
	state, err := m.FrontDoorState()
	if err != nil {
		t.Fatal(err)
	}
	return state.Claim
}

// ---- tests ------------------------------------------------------------------

func TestManagerClaimsAnAccountAndAdvancesTheCursor(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	run(t, m)

	claim, err := m.IssueClaim()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first poll", func() bool { return m.Snapshot().Running })
	fb.push(privateMessage(7, 500, "/claim "+claim.Code))

	waitFor(t, "the claim to bind", func() bool {
		_, ok := st.TelegramAllowed(500)
		return ok
	})
	bound, _ := st.TelegramAllowed(500)
	if bound.ChatID != 500 || bound.Username != "ada" || bound.ClaimedAt.IsZero() {
		t.Fatalf("bound %+v", bound)
	}
	waitFor(t, "the welcome reply", func() bool { return len(fb.replies()) == 1 })
	if replies := fb.replies(); !strings.Contains(replies[0], "Connected") {
		t.Fatalf("replies %q", replies)
	}
	// The update is acknowledged, so Telegram does not hand it back forever.
	waitFor(t, "the cursor to advance", func() bool { return st.TelegramLink().Offset == 8 })
	if fb.askedOffsets()[0] != "" {
		t.Fatalf("the first poll must ask from the start, got %q", fb.askedOffsets()[0])
	}
	// The code is single use.
	if claim := pendingClaim(t, m); claim.Code != "" {
		t.Fatalf("code survived redemption: %+v", claim)
	}
}

func TestManagerRefusesToBindWithoutAValidCode(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	run(t, m)

	claim, err := m.IssueClaim()
	if err != nil {
		t.Fatal(err)
	}
	wrong := strings.Replace(claim.Code, string(claim.Code[0]), "Z", 1)
	if wrong == claim.Code {
		wrong = "ZZZZZZZZ"
	}
	// claimAttempts wrong guesses burn the pending code, so guessing cannot be
	// run as an online search.
	for i := 0; i < claimAttempts; i++ {
		fb.push(privateMessage(int64(10+i), 500, "/claim "+wrong))
	}
	waitFor(t, "every guess to be answered", func() bool { return len(fb.replies()) == claimAttempts })
	if _, ok := st.TelegramAllowed(500); ok {
		t.Fatal("a wrong code must not bind an account")
	}
	if claim := pendingClaim(t, m); claim.Code != "" {
		t.Fatalf("the code should be burnt after %d guesses: %+v", claimAttempts, claim)
	}
}

// The allow-list is the only thing standing between a stranger and the agent,
// so empty must mean deny.
func TestManagerDeniesEveryoneUntilSomeoneClaims(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	called := 0
	m.Handle = func(context.Context, Message) error { called++; return nil }
	run(t, m)

	fb.push(privateMessage(1, 999, "hello, are you there?"))
	waitFor(t, "the update to be consumed", func() bool { return st.TelegramLink().Offset == 2 })
	if called != 0 {
		t.Fatal("a stranger's message reached the worker")
	}
	// Silence for anything but /start: answering every unknown sender would
	// make the bot a megaphone.
	if replies := fb.replies(); len(replies) != 0 {
		t.Fatalf("a stranger should get no reply, got %q", replies)
	}

	fb.push(privateMessage(2, 999, "/start"))
	waitFor(t, "the instructions", func() bool { return len(fb.replies()) == 1 })
	if !strings.Contains(fb.replies()[0], "claim code") {
		t.Fatalf("instructions %q", fb.replies()[0])
	}
}

func TestManagerDeliversFromAllowedAccountsAndRefusesGroups(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken)
	if err := st.UpsertTelegramUser(store.TelegramUser{UserID: 500, ChatID: 500, Username: "old", ClaimedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	var got []Message
	m.Handle = func(_ context.Context, msg Message) error { got = append(got, msg); return nil }
	run(t, m)

	fb.push(privateMessage(1, 500, "run the tests"))
	waitFor(t, "delivery", func() bool { return len(got) == 1 })
	if got[0].Text != "run the tests" || got[0].From.ID != 500 {
		t.Fatalf("delivered %+v", got[0])
	}
	// The stored name is refreshed from the live update, so the dashboard shows
	// what the account calls itself now.
	if known, _ := st.TelegramAllowed(500); known.Username != "ada" {
		t.Fatalf("username not refreshed: %+v", known)
	}

	// A group chat is refused: the chat id would be the group, so binding it
	// would let any member act as the owner.
	group := Update{UpdateID: 2, Message: &Message{
		MessageID: 2, Text: "hello from the group",
		Chat: Chat{ID: -100123, Type: "group", Title: "Team"},
		From: &User{ID: 500, Username: "ada"},
	}}
	fb.push(group)
	waitFor(t, "the group update to be consumed", func() bool { return st.TelegramLink().Offset == 3 })
	if len(got) != 1 {
		t.Fatalf("a group message must not be delivered, got %d", len(got))
	}
}

// Without a worker attached the front door still answers, so the whole chain can
// be verified before delivery exists.
func TestManagerAcknowledgesWhenNoWorkerIsAttached(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken)
	if err := st.UpsertTelegramUser(store.TelegramUser{UserID: 500, ChatID: 500, ClaimedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	run(t, m)

	fb.push(privateMessage(1, 500, "hello"))
	waitFor(t, "the acknowledgement", func() bool { return len(fb.replies()) == 1 })
	if !strings.Contains(fb.replies()[0], "No worker is attached") {
		t.Fatalf("reply %q", fb.replies()[0])
	}
}

// The cursor is the one piece of runtime state that must outlive a restart.
func TestManagerResumesFromTheStoredCursor(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken)
	if err := st.SetTelegramOffset(50); err != nil {
		t.Fatal(err)
	}
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	run(t, m)

	fb.push(privateMessage(50, 999, "hello"))
	waitFor(t, "the cursor to advance", func() bool { return st.TelegramLink().Offset == 51 })
	if offsets := fb.askedOffsets(); len(offsets) == 0 || offsets[0] != "50" {
		t.Fatalf("the poll must resume from the stored cursor, asked %v", offsets)
	}
}

func TestManagerSurvivesATelegramFailure(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	fb.fail = true
	m := NewManager(st, fb.client())
	run(t, m)

	waitFor(t, "the failure to be reported", func() bool {
		snap := m.Snapshot()
		return !snap.Running && snap.LastError != ""
	})
	if snap := m.Snapshot(); strings.Contains(snap.LastError, goodToken) {
		t.Fatalf("the failure leaked the token: %s", snap.LastError)
	}
}

// A webhook on the bot makes getUpdates fail with 409, so taking the bot over
// for polling has to clear one.
func TestManagerClearsAWebhookBeforePolling(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	run(t, m)

	waitFor(t, "the poll to start", func() bool { return m.Snapshot().Running })
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if fb.hookCall != 1 {
		t.Fatalf("want exactly one deleteWebhook, got %d", fb.hookCall)
	}
}

func TestManagerStopsWhenTheTokenIsRemoved(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	run(t, m)
	waitFor(t, "the poll to start", func() bool { return m.Snapshot().Running })

	if err := st.DeleteSecret("sec_test"); err != nil {
		t.Fatal(err)
	}
	m.Kick()
	waitFor(t, "the poller to stop", func() bool { return !m.Snapshot().Running })
}

func TestManagerDoesNotPollWithoutAToken(t *testing.T) {
	st := newStore(t)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	run(t, m)

	time.Sleep(20 * time.Millisecond)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if fb.meCalls != 0 {
		t.Fatalf("a gateway with no bot must not call Telegram, got %d getMe", fb.meCalls)
	}
	if snap := m.Snapshot(); snap.Running {
		t.Fatalf("status %+v", snap)
	}
}

func TestManagerHidesAnExpiredClaimCode(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken)
	now := time.Unix(1700000000, 0)
	m := NewManager(st, newFakeBot(42, "piso_bot").client())
	m.now = func() time.Time { return now }

	claim, err := m.IssueClaim()
	if err != nil {
		t.Fatal(err)
	}
	if got := pendingClaim(t, m); got.Code != claim.Code {
		t.Fatalf("the pending code must be visible: %+v", got)
	}
	now = now.Add(ClaimTTL + time.Second)
	if got := pendingClaim(t, m); got.Code != "" {
		t.Fatalf("an expired code must not be offered: %+v", got)
	}
}

func TestManagerIssuesOneCodeAtATime(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken)
	m := NewManager(st, newFakeBot(42, "piso_bot").client())

	first, err := m.IssueClaim()
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.IssueClaim()
	if err != nil {
		t.Fatal(err)
	}
	if first.Code == second.Code {
		t.Fatalf("want a fresh code, got %q twice", first.Code)
	}
	if claim := pendingClaim(t, m); claim.Code != second.Code {
		t.Fatalf("only the newest code may be live: %+v", claim)
	}
}

func TestManagerCannotIssueACodeWithoutABot(t *testing.T) {
	m := NewManager(newStore(t), newFakeBot(42, "piso_bot").client())
	if _, err := m.IssueClaim(); err == nil {
		t.Fatal("want an error when no bot token is stored")
	}
}

func TestClaimCodeParsing(t *testing.T) {
	code, _ := randomCode()
	if len(code) != claimLength {
		t.Fatalf("code %q", code)
	}
	for _, r := range code {
		if !strings.ContainsRune(claimAlphabet, r) {
			t.Fatalf("code %q contains an ambiguous character", code)
		}
	}
	// A code is read off a screen and typed, so case and the grouping dash the
	// dashboard shows are not significant.
	shown := code[:claimLength/2] + "-" + code[claimLength/2:]
	for _, in := range []string{"/claim " + code, "/claim " + shown, "/claim " + strings.ToLower(code), "/CLAIM " + shown} {
		got, ok := claimCode(in)
		if !ok || got != code {
			t.Fatalf("claimCode(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"/claim", "/claim ", "/claim nope", "/claim 1234567", "/claim 123456789", "claim " + code, "/claim " + code + " extra"} {
		if got, ok := claimCode(in); ok {
			t.Fatalf("claimCode(%q) accepted %q", in, got)
		}
	}
}

// ---- poller lease -----------------------------------------------------------

// addPublishedToken stores a bot token that has a coordinator resource id, so
// the manager must ask for the poller lease before it polls.
func addPublishedToken(t *testing.T, st *store.Store, token string) {
	t.Helper()
	if err := st.AddSecret(store.SecretRec{
		ID: "sec_test", Name: "Telegram bot", Kind: model.SecretKindTelegram,
		GatewayOnly: true, Value: token,
		TechnocoreID: "11111111-1111-1111-1111-111111111111",
	}); err != nil {
		t.Fatal(err)
	}
}

// fakeClock returns a controllable clock plus an advance function. The lease
// renewal cadence is measured with it, so a test does not have to wait a
// minute for the next renewal.
func fakeClock(m *Manager) (advance func(time.Duration)) {
	var mu sync.Mutex
	now := time.Now()
	m.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	return func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
}

func TestManagerRefusedTheLeaseNeverPolls(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	var asks int32
	m.Lease = func(_ context.Context, resourceID string, _ int64) (LeaseGrant, error) {
		atomic.AddInt32(&asks, 1)
		if resourceID != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("lease asked for resource %q", resourceID)
		}
		return LeaseGrant{Granted: false, Holder: "other"}, nil
	}
	run(t, m)

	waitFor(t, "the lease to be asked for", func() bool { return atomic.LoadInt32(&asks) > 0 })
	time.Sleep(30 * time.Millisecond)
	fb.mu.Lock()
	me, hook := fb.meCalls, fb.hookCall
	fb.mu.Unlock()
	if me != 0 || hook != 0 {
		t.Fatalf("a gateway without the lease must not talk to Telegram: getMe=%d deleteWebhook=%d", me, hook)
	}
	snap := m.Snapshot()
	if snap.Running || !snap.Waiting || snap.LeaseHeld || snap.LastError != "" {
		t.Fatalf("a refusal is a wait, not an error: %+v", snap)
	}
}

func TestManagerPollsAnUnpublishedBotWithoutALease(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken) // no coordinator resource id
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	var asks int32
	m.Lease = func(_ context.Context, _ string, _ int64) (LeaseGrant, error) {
		atomic.AddInt32(&asks, 1)
		return LeaseGrant{Granted: false}, nil
	}
	run(t, m)

	waitFor(t, "the poll to start", func() bool { return m.Snapshot().Running })
	if n := atomic.LoadInt32(&asks); n != 0 {
		t.Fatalf("asked for a lease on a bot that was never published: %d asks", n)
	}
}

func TestManagerResumesFromTheLeasedCursor(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	m.Lease = func(_ context.Context, _ string, offset int64) (LeaseGrant, error) {
		if offset != 0 {
			t.Errorf("first ask reported offset %d, want the empty local cursor", offset)
		}
		return LeaseGrant{Granted: true, Offset: 100, ExpiresAt: time.Now().Add(time.Minute)}, nil
	}
	run(t, m)

	waitFor(t, "the poll to start", func() bool { return m.Snapshot().Running })
	if snap := m.Snapshot(); !snap.LeaseHeld || snap.Waiting {
		t.Fatalf("status %+v", snap)
	}
	waitFor(t, "the first getUpdates", func() bool { return len(fb.askedOffsets()) > 0 })
	if got := fb.askedOffsets()[0]; got != "100" {
		t.Fatalf("first getUpdates offset %q, want the coordinator's 100", got)
	}
}

func TestManagerRenewsWithTheAdvancedCursor(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	advance := fakeClock(m)
	var mu sync.Mutex
	var asks []int64
	m.Lease = func(_ context.Context, _ string, offset int64) (LeaseGrant, error) {
		mu.Lock()
		asks = append(asks, offset)
		mu.Unlock()
		return LeaseGrant{Granted: true, ExpiresAt: time.Now().Add(time.Minute)}, nil
	}
	run(t, m)

	waitFor(t, "the poll to start", func() bool { return m.Snapshot().Running })
	fb.push(privateMessage(9, 500, "/start"))
	waitFor(t, "the cursor to advance", func() bool { return st.TelegramLink().Offset == 10 })

	// Unblock the in-flight poll once the renewal is due: the loop re-checks
	// the lease between getUpdates calls.
	advance(leaseRenew)
	fb.push()
	waitFor(t, "a renewal", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(asks) >= 2
	})
	mu.Lock()
	defer mu.Unlock()
	if got := asks[len(asks)-1]; got != 10 {
		t.Fatalf("renewal reported offset %d, want the advanced cursor 10", got)
	}
}

func TestManagerStopsPollingWhenAnotherGatewayTakesTheLease(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	advance := fakeClock(m)
	var mu sync.Mutex
	calls := 0
	m.Lease = func(_ context.Context, _ string, _ int64) (LeaseGrant, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return LeaseGrant{Granted: true, ExpiresAt: time.Now().Add(time.Minute)}, nil
		}
		return LeaseGrant{Granted: false, Holder: "other"}, nil
	}
	run(t, m)

	waitFor(t, "the poll to start", func() bool { return m.Snapshot().Running })
	advance(leaseRenew)
	fb.push()
	waitFor(t, "the loss to be noticed", func() bool {
		snap := m.Snapshot()
		return !snap.Running && snap.Waiting
	})
	if snap := m.Snapshot(); snap.LastError != "" || snap.LeaseHeld {
		t.Fatalf("a lease taken by another gateway is a wait, not an error: %+v", snap)
	}
}

// A bot that is published mid-session becomes visible to peers, so the session
// polling without a lease must stop and re-acquire under one.
func TestManagerAsksForTheLeaseOnceTheBotIsPublished(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken) // no coordinator resource id yet
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	var asks int32
	m.Lease = func(_ context.Context, _ string, _ int64) (LeaseGrant, error) {
		atomic.AddInt32(&asks, 1)
		return LeaseGrant{Granted: true, ExpiresAt: time.Now().Add(time.Minute)}, nil
	}
	run(t, m)

	waitFor(t, "the poll to start", func() bool { return m.Snapshot().Running })
	if n := atomic.LoadInt32(&asks); n != 0 {
		t.Fatalf("asked for a lease before the bot was published: %d asks", n)
	}
	rec, ok := Secret(st.Secrets())
	if !ok {
		t.Fatal("no telegram secret stored")
	}
	rec.TechnocoreID = "11111111-1111-1111-1111-111111111111"
	if err := st.ReplaceSecret(rec); err != nil {
		t.Fatal(err)
	}
	fb.push() // unblock the poll so the session re-checks the stored secret
	waitFor(t, "the lease to be asked for", func() bool { return atomic.LoadInt32(&asks) > 0 })
	waitFor(t, "the leased poll to run", func() bool {
		snap := m.Snapshot()
		return snap.Running && snap.LeaseHeld
	})
}
