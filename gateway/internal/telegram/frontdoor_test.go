package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"piso/gateway/internal/store"
)

// stubDoor is a scripted coordinator front door. It is mutex-guarded because
// the manager refetches from its own goroutine while a test edits it.
type stubDoor struct {
	mu      sync.Mutex
	users   []store.TelegramUser
	claim   Claim
	allow   bool
	fail    error
	reads   int
	issues  int
	redeems []string
	revoked []int64
}

func (s *stubDoor) setUsers(users ...store.TelegramUser) {
	s.mu.Lock()
	s.users = append([]store.TelegramUser(nil), users...)
	s.mu.Unlock()
}

func (s *stubDoor) all() []store.TelegramUser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.TelegramUser(nil), s.users...)
}

func (s *stubDoor) counts() (reads, issues int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads, s.issues
}

func (s *stubDoor) redeemCodes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.redeems...)
}

func (s *stubDoor) revokedIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.revoked...)
}

func (s *stubDoor) State(string) (FrontDoorState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.fail != nil {
		return FrontDoorState{}, s.fail
	}
	return FrontDoorState{Users: s.users, Claim: s.claim}, nil
}

func (s *stubDoor) Issue(string) (Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues++
	if s.fail != nil {
		return Claim{}, s.fail
	}
	return s.claim, nil
}

func (s *stubDoor) Redeem(_ string, code string, u store.TelegramUser) (bool, []store.TelegramUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redeems = append(s.redeems, code)
	if s.fail != nil {
		return false, nil, s.fail
	}
	if !s.allow {
		return false, s.users, nil
	}
	s.users = append(append([]store.TelegramUser(nil), s.users...), u)
	return true, s.users, nil
}

func (s *stubDoor) Upsert(_ string, u store.TelegramUser) ([]store.TelegramUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	for i := range s.users {
		if s.users[i].UserID == u.UserID {
			s.users[i] = u
		}
	}
	return s.users, nil
}

func (s *stubDoor) Revoke(_ string, userID int64) (bool, []store.TelegramUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = append(s.revoked, userID)
	if s.fail != nil {
		return false, nil, s.fail
	}
	kept := make([]store.TelegramUser, 0, len(s.users))
	found := false
	for _, u := range s.users {
		if u.UserID == userID {
			found = true
			continue
		}
		kept = append(kept, u)
	}
	if !found {
		return false, nil, nil
	}
	s.users = kept
	return true, kept, nil
}

func TestManagerUsesTheCoordinatorFrontDoorWhenPublished(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	m := NewManager(st, newFakeBot(42, "piso_bot").client())
	door := &stubDoor{users: []store.TelegramUser{{UserID: 7, ChatID: 7, Username: "ada"}}}
	m.Remote = newRemoteFrontDoor(st, door)

	if err := m.Refresh(); err != nil {
		t.Fatal(err)
	}
	if got := st.TelegramLink().Users; len(got) != 1 || got[0].UserID != 7 {
		t.Fatalf("the coordinator list was not cached: %+v", got)
	}
	if _, ok := st.TelegramAllowed(7); !ok {
		t.Fatal("a coordinator-approved account must be allowed")
	}
}

func TestManagerIgnoresTheCoordinatorForAnUnpublishedBot(t *testing.T) {
	st := newStore(t)
	addToken(t, st, goodToken) // never published
	m := NewManager(st, newFakeBot(42, "piso_bot").client())
	door := &stubDoor{}
	m.Remote = newRemoteFrontDoor(st, door)

	if err := m.Refresh(); err != nil {
		t.Fatal(err)
	}
	if reads, _ := door.counts(); reads != 0 {
		t.Fatalf("an unpublished bot must not read the coordinator: %d reads", reads)
	}
}

func TestManagerRedeemsThroughTheCoordinator(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	door := &stubDoor{allow: true}
	m.Remote = newRemoteFrontDoor(st, door)
	run(t, m)

	// The code lives on the coordinator, so the holder accepts a code it has
	// never seen locally.
	fb.push(privateMessage(5, 500, "/claim ABCDEFGH"))
	waitFor(t, "the redeem", func() bool { return len(door.redeemCodes()) == 1 })
	if got := door.redeemCodes()[0]; got != "ABCDEFGH" {
		t.Fatalf("redeemed %q", got)
	}
	waitFor(t, "the bind to be cached", func() bool {
		_, ok := st.TelegramAllowed(500)
		return ok
	})
	waitFor(t, "the welcome reply", func() bool { return len(fb.replies()) == 1 })
	if reply := fb.replies()[0]; !strings.Contains(reply, "Connected") {
		t.Fatalf("reply %q", reply)
	}
}

func TestManagerRefreshesWhenTheCoordinatorPushes(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	m := NewManager(st, newFakeBot(42, "piso_bot").client())
	door := &stubDoor{users: []store.TelegramUser{{UserID: 1, ChatID: 1}}}
	m.Remote = newRemoteFrontDoor(st, door)
	run(t, m)

	waitFor(t, "the first refresh", func() bool { _, ok := st.TelegramAllowed(1); return ok })

	// A revocation elsewhere arrives as a push, not a poll.
	door.setUsers(store.TelegramUser{UserID: 2, ChatID: 2})
	st.BroadcastTelegram()
	waitFor(t, "the pushed list to replace the cache", func() bool {
		if _, ok := st.TelegramAllowed(1); ok {
			return false
		}
		_, ok := st.TelegramAllowed(2)
		return ok
	})
}

func TestManagerRefreshesWhenItTakesTheLease(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	m := NewManager(st, newFakeBot(42, "piso_bot").client())
	door := &stubDoor{users: []store.TelegramUser{{UserID: 9, ChatID: 9}}}
	m.Remote = newRemoteFrontDoor(st, door)
	m.Lease = func(_ context.Context, _ string, _ int64) (LeaseGrant, error) {
		return LeaseGrant{Granted: true, ExpiresAt: time.Now().Add(time.Minute)}, nil
	}
	run(t, m)

	// Taking the lease must not enforce the list inherited from the previous
	// holder: the coordinator's is fetched first.
	waitFor(t, "the list to be fetched on acquire", func() bool {
		_, ok := st.TelegramAllowed(9)
		return ok
	})
}

func TestManagerRevokeGoesThroughTheCoordinator(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	m := NewManager(st, newFakeBot(42, "piso_bot").client())
	door := &stubDoor{users: []store.TelegramUser{{UserID: 7, ChatID: 7}}}
	m.Remote = newRemoteFrontDoor(st, door)
	if err := m.Refresh(); err != nil {
		t.Fatal(err)
	}

	removed, err := m.Revoke(7)
	if err != nil || !removed {
		t.Fatalf("revoke %v %v", removed, err)
	}
	if got := door.revokedIDs(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("revoked %v", got)
	}
	if _, ok := st.TelegramAllowed(7); ok {
		t.Fatal("a revoked account must not stay in the cache")
	}
}

func TestManagerKeepsTheCacheWhenARevokeMisses(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	m := NewManager(st, newFakeBot(42, "piso_bot").client())
	door := &stubDoor{users: []store.TelegramUser{{UserID: 7, ChatID: 7}}}
	m.Remote = newRemoteFrontDoor(st, door)
	if err := m.Refresh(); err != nil {
		t.Fatal(err)
	}

	removed, err := m.Revoke(99)
	if err != nil || removed {
		t.Fatalf("revoke %v %v", removed, err)
	}
	// A miss reports no list. That must not be read as "nobody is allowed".
	if _, ok := st.TelegramAllowed(7); !ok {
		t.Fatal("a missed revoke cleared the cached allow-list")
	}
}

func TestManagerFallsBackToTheCacheWhenTheCoordinatorFails(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	m := NewManager(st, newFakeBot(42, "piso_bot").client())
	door := &stubDoor{fail: errors.New("coordinator down")}
	m.Remote = newRemoteFrontDoor(st, door)
	if err := st.SetTelegramUsers([]store.TelegramUser{{UserID: 3, ChatID: 3}}); err != nil {
		t.Fatal(err)
	}

	if err := m.Refresh(); err == nil {
		t.Fatal("a failed refresh must report the error")
	}
	if _, ok := st.TelegramAllowed(3); !ok {
		t.Fatal("a failed refresh must leave the cache alone")
	}
	// The dashboard falls back to the cached view rather than showing nobody.
	local := m.LocalFrontDoorState()
	if len(local.Users) != 1 || local.Users[0].UserID != 3 {
		t.Fatalf("local state %+v", local)
	}
}
