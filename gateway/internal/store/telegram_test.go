package store

import (
	"path/filepath"
	"testing"
	"time"
)

func telegramStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := New(
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

func TestTelegramLinkRoundTripsThroughDisk(t *testing.T) {
	st := telegramStore(t)
	claimed := time.Unix(1700000000, 0).UTC()
	if err := st.SetTelegramLink(TelegramLink{
		BotID:  42,
		Offset: 7,
		Users:  []TelegramUser{{UserID: 500, ChatID: 500, Username: "ada", ClaimedAt: claimed}},
	}); err != nil {
		t.Fatal(err)
	}
	// A restart is a fresh Store over the same path; the cursor and the
	// allow-list have to survive it or the bot replays or drops updates.
	dir := filepath.Dir(st.Path())
	st.Close()
	reopened, err := New(
		filepath.Join(dir, "state.json"),
		filepath.Join(dir, "log.jsonl"),
		filepath.Join(dir, "patterns.json"),
		filepath.Join(dir, "activities.db"),
		20,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })

	link := reopened.TelegramLink()
	if link.BotID != 42 || link.Offset != 7 || len(link.Users) != 1 {
		t.Fatalf("link %+v", link)
	}
	if !link.Users[0].ClaimedAt.Equal(claimed) {
		t.Fatalf("claimedAt %s", link.Users[0].ClaimedAt)
	}
}

// TelegramLink is read by the poller on one goroutine while the dashboard
// writes it from another, so the returned slice must not alias the state.
func TestTelegramLinkReturnsACopy(t *testing.T) {
	st := telegramStore(t)
	if err := st.SetTelegramLink(TelegramLink{Users: []TelegramUser{{UserID: 500}}}); err != nil {
		t.Fatal(err)
	}
	link := st.TelegramLink()
	link.Users[0].UserID = 999
	if got, _ := st.TelegramAllowed(500); got.UserID != 500 {
		t.Fatalf("the caller mutated stored state: %+v", got)
	}
}

func TestTelegramOffsetAdvancesWithoutTouchingTheAllowList(t *testing.T) {
	st := telegramStore(t)
	if err := st.SetTelegramLink(TelegramLink{BotID: 42, Users: []TelegramUser{{UserID: 500}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTelegramOffset(51); err != nil {
		t.Fatal(err)
	}
	link := st.TelegramLink()
	if link.Offset != 51 || link.BotID != 42 || len(link.Users) != 1 {
		t.Fatalf("link %+v", link)
	}
}

func TestTelegramAllowedIsExact(t *testing.T) {
	st := telegramStore(t)
	if err := st.UpsertTelegramUser(TelegramUser{UserID: 500, ChatID: 500}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.TelegramAllowed(500); !ok {
		t.Fatal("a bound account must be allowed")
	}
	for _, id := range []int64{0, 499, 501, -500} {
		if _, ok := st.TelegramAllowed(id); ok {
			t.Fatalf("account %d must not be allowed", id)
		}
	}
}

// The stored name is a label, so a later message refreshes it; the claim time is
// provenance, so it must not move.
func TestUpsertTelegramUserRefreshesTheNameAndKeepsTheClaimTime(t *testing.T) {
	st := telegramStore(t)
	claimed := time.Unix(1700000000, 0).UTC()
	if err := st.UpsertTelegramUser(TelegramUser{UserID: 500, ChatID: 500, Username: "old", FirstName: "Old", ClaimedAt: claimed}); err != nil {
		t.Fatal(err)
	}
	later := time.Unix(1800000000, 0).UTC()
	if err := st.UpsertTelegramUser(TelegramUser{UserID: 500, ChatID: 500, Username: "ada", FirstName: "Ada", ClaimedAt: later}); err != nil {
		t.Fatal(err)
	}
	users := st.TelegramLink().Users
	if len(users) != 1 {
		t.Fatalf("want one account, got %+v", users)
	}
	if users[0].Username != "ada" || users[0].FirstName != "Ada" {
		t.Fatalf("name not refreshed: %+v", users[0])
	}
	if !users[0].ClaimedAt.Equal(claimed) {
		t.Fatalf("claimedAt moved to %s", users[0].ClaimedAt)
	}
}

func TestRemoveTelegramUserRevokesExactlyOne(t *testing.T) {
	st := telegramStore(t)
	for _, id := range []int64{500, 501} {
		if err := st.UpsertTelegramUser(TelegramUser{UserID: id, ChatID: id}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := st.RemoveTelegramUser(500)
	if err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if _, ok := st.TelegramAllowed(500); ok {
		t.Fatal("a revoked account must be denied")
	}
	if _, ok := st.TelegramAllowed(501); !ok {
		t.Fatal("revoking one account must not touch another")
	}
	if removed, err := st.RemoveTelegramUser(500); err != nil || removed {
		t.Fatalf("revoking twice = %v, %v", removed, err)
	}
}
