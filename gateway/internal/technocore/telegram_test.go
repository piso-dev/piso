package technocore

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"piso/gateway/internal/store"
)

// doorCoordinator answers the front-door endpoints and records what was sent,
// so a test can assert the signed path, method and body.
type doorCoordinator struct {
	mu       sync.Mutex
	requests []string
	handler  func(method, path string) (int, string)
}

func newDoorCoordinator(handler func(method, path string) (int, string)) *doorCoordinator {
	return &doorCoordinator{handler: handler}
}

func (d *doorCoordinator) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.requests = append(d.requests, r.Method+" "+r.URL.Path+" "+string(raw))
		handler := d.handler
		d.mu.Unlock()
		status, body := handler(r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			http.Error(w, body, status)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

func (d *doorCoordinator) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...)
}

func doorEnv(t *testing.T, d *doorCoordinator) string {
	t.Helper()
	srv := d.server()
	t.Cleanup(srv.Close)
	_, dataDir, _ := enrolledShareEnv(t, srv.URL)
	old := serverHTTP
	serverHTTP = srv.Client()
	t.Cleanup(func() { serverHTTP = old })
	return dataDir
}

func TestTelegramFrontDoorReadsTheAllowListAndClaim(t *testing.T) {
	coord := newDoorCoordinator(func(method, path string) (int, string) {
		switch {
		case strings.HasSuffix(path, "/front-door"):
			return 200, `{"users":[{"userId":7,"chatId":7,"username":"ada","claimedAt":"2026-01-02T03:04:05Z"}],` +
				`"claim":{"code":"ABCDEFGH","expires_at":"2026-01-02T03:14:05.123456+00:00"}}`
		case strings.HasSuffix(path, "/claim"):
			return 200, `{"code":"ABCDEFGH","expires_at":"2026-01-02T03:14:05+00:00"}`
		}
		return 500, "unexpected " + path
	})
	dataDir := doorEnv(t, coord)

	state, err := TelegramFrontDoor(dataDir, "rid-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Users) != 1 || state.Users[0].UserID != 7 || state.Users[0].Username != "ada" {
		t.Fatalf("users %+v", state.Users)
	}
	if state.Users[0].ClaimedAt.IsZero() {
		t.Fatal("claimedAt did not parse")
	}
	if state.Claim == nil || state.Claim.Code != "ABCDEFGH" || state.Claim.ExpiresAt.IsZero() {
		t.Fatalf("claim %+v", state.Claim)
	}

	claim, err := TelegramIssueClaim(dataDir, "rid-1")
	if err != nil || claim.Code != "ABCDEFGH" || claim.ExpiresAt.IsZero() {
		t.Fatalf("issue %+v %v", claim, err)
	}

	// The resource id rides in the path because the gateway signature covers
	// the path and not the query.
	want := []string{
		"GET /api/v1/telegram/bots/rid-1/front-door ",
		"POST /api/v1/telegram/bots/rid-1/claim {}",
	}
	if got := coord.seen(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests %q, want %q", got, want)
	}
}

func TestTelegramFrontDoorRedeemUpsertAndRevoke(t *testing.T) {
	coord := newDoorCoordinator(func(method, path string) (int, string) {
		switch {
		case strings.HasSuffix(path, "/redeem"):
			return 200, `{"allowed":true,"users":[{"userId":7,"chatId":7,"username":"ada"}]}`
		case method == http.MethodDelete:
			return 200, `{"users":[]}`
		default:
			return 200, `{"users":[{"userId":7,"chatId":7,"username":"ada2"}]}`
		}
	})
	dataDir := doorEnv(t, coord)

	allowed, users, err := TelegramRedeemClaim(dataDir, "rid-1", "ABCDEFGH", store.TelegramUser{UserID: 7, ChatID: 7})
	if err != nil || !allowed || len(users) != 1 {
		t.Fatalf("redeem %v %+v %v", allowed, users, err)
	}

	found, users, err := TelegramRevokeUser(dataDir, "rid-1", 7)
	if err != nil || !found || users == nil || len(users) != 0 {
		t.Fatalf("revoke %v %+v %v", found, users, err)
	}

	users, err = TelegramUpsertUser(dataDir, "rid-1", store.TelegramUser{UserID: 7, Username: "ada2"})
	if err != nil || len(users) != 1 || users[0].Username != "ada2" {
		t.Fatalf("upsert %+v %v", users, err)
	}

	seen := coord.seen()
	if len(seen) != 3 {
		t.Fatalf("requests %q", seen)
	}
	if !strings.HasPrefix(seen[0], "POST /api/v1/telegram/bots/rid-1/claim/redeem ") ||
		!strings.Contains(seen[0], `"code":"ABCDEFGH"`) ||
		!strings.Contains(seen[0], `"userId":7`) {
		t.Fatalf("redeem request %q", seen[0])
	}
	if !strings.HasPrefix(seen[1], "DELETE /api/v1/telegram/bots/rid-1/users/7") {
		t.Fatalf("revoke request %q", seen[1])
	}
	if !strings.HasPrefix(seen[2], "PUT /api/v1/telegram/bots/rid-1/users/7 ") ||
		!strings.Contains(seen[2], `"firstName"`) {
		t.Fatalf("upsert request %q", seen[2])
	}
}

func TestTelegramRevokeMissIsNotAFailure(t *testing.T) {
	coord := newDoorCoordinator(func(method, path string) (int, string) {
		return http.StatusNotFound, "that account is not in the allow-list"
	})
	dataDir := doorEnv(t, coord)

	found, users, err := TelegramRevokeUser(dataDir, "rid-1", 7)
	if err != nil || found || users != nil {
		t.Fatalf("revoke %v %+v %v", found, users, err)
	}
}

func TestTelegramFrontDoorRejectsNon200(t *testing.T) {
	coord := newDoorCoordinator(func(method, path string) (int, string) {
		return http.StatusForbidden, "not a recipient"
	})
	dataDir := doorEnv(t, coord)

	if _, err := TelegramFrontDoor(dataDir, "rid-1"); err == nil {
		t.Fatal("expected an error for a forbidden front-door read")
	}
	if _, err := TelegramIssueClaim(dataDir, "rid-1"); err == nil {
		t.Fatal("expected an error for a forbidden claim")
	}
}
