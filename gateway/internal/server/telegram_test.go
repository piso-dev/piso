package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
	"piso/gateway/internal/telegram"
)

const botToken = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

// stubBotAPI points the server's outbound client, and the poller's, at an
// in-process Bot API.
func stubBotAPI(t *testing.T, handler http.HandlerFunc) *Server {
	t.Helper()
	s := testServer(t)
	s.HTTP = &http.Client{Transport: handlerTransport{handler: handler}}
	s.Telegram.HTTP = s.HTTP
	return s
}

// botAPI answers getMe for one bot. Everything else is a test failure: the
// connect path must not call any other method.
func botAPI(t *testing.T, id int64, token, username string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot"+token+"/getMe" {
			t.Errorf("unexpected call %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"id":%d,"is_bot":true,"first_name":"Piso","username":%q}}`, id, username)
	}
}

func telegramSecretOf(t *testing.T, s *Server) (store.SecretRec, bool) {
	t.Helper()
	return telegram.Secret(s.Store.Secrets())
}

func TestTelegramConnectStoresAGatewayOnlySecret(t *testing.T) {
	s := stubBotAPI(t, botAPI(t, 42, botToken, "piso_bot"))
	ch := s.ControlHandler()

	if body := doJSON(t, ch, "GET", "/api/v1/telegram", nil).Body.String(); !strings.Contains(body, `"connected":false`) {
		t.Fatalf("want disconnected before connect, got %s", body)
	}

	w := doJSON(t, ch, "POST", "/api/v1/telegram", map[string]any{"token": botToken})
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"botUsername":"piso_bot"`) {
		t.Fatalf("connect %d %s", w.Code, w.Body.String())
	}

	// The stored secret is gateway-local: no placeholder, no env key, and no
	// substitution rule, so nothing can reach a worker.
	rec, ok := telegramSecretOf(t, s)
	if !ok {
		t.Fatal("no telegram secret stored")
	}
	if rec.Kind != model.SecretKindTelegram || !rec.GatewayOnly || rec.Value != botToken {
		t.Fatalf("secret %+v", rec)
	}
	if rec.Placeholder != "" || rec.EnvKey != "" || rec.AllowedHosts != nil {
		t.Fatalf("gateway-only secret must have no worker-facing fields: %+v", rec)
	}
	if rules := s.Store.Rules(); len(rules) != 0 {
		t.Fatalf("gateway-only secret must seed no rules, got %+v", rules)
	}

	// It still shows up in the secrets list, marked as its own kind.
	listed := doJSON(t, ch, "GET", "/api/v1/secrets", nil).Body.String()
	if !strings.Contains(listed, `"kind":"telegram"`) {
		t.Fatalf("secrets list missing telegram kind: %s", listed)
	}

	status := doJSON(t, ch, "GET", "/api/v1/telegram", nil).Body.String()
	if !strings.Contains(status, `"connected":true`) || !strings.Contains(status, `"botId":42`) {
		t.Fatalf("status %s", status)
	}
	// The token is never echoed, on any route.
	for _, body := range []string{status, listed} {
		if strings.Contains(body, botToken) {
			t.Fatalf("token leaked: %s", body)
		}
	}

	if d := doJSON(t, ch, "DELETE", "/api/v1/telegram", nil); d.Code != 204 {
		t.Fatalf("disconnect %d %s", d.Code, d.Body.String())
	}
	if _, ok := telegramSecretOf(t, s); ok {
		t.Fatal("secret survived disconnect")
	}
	if body := doJSON(t, ch, "GET", "/api/v1/telegram", nil).Body.String(); !strings.Contains(body, `"connected":false`) {
		t.Fatalf("want disconnected after delete, got %s", body)
	}
}

func TestTelegramConnectRejectsAMalformedToken(t *testing.T) {
	s := stubBotAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("a malformed token must not reach Telegram")
	})
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/telegram", map[string]any{"token": "  nope  "})
	if w.Code != 400 {
		t.Fatalf("want 400, got %d %s", w.Code, w.Body.String())
	}
	if _, ok := telegramSecretOf(t, s); ok {
		t.Fatal("a rejected token must not be persisted")
	}
}

func TestTelegramConnectReportsATelegramRejection(t *testing.T) {
	s := stubBotAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
	})
	w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/telegram", map[string]any{"token": botToken})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Unauthorized") {
		t.Fatalf("want Telegram's description, got %d %s", w.Code, w.Body.String())
	}
	if _, ok := telegramSecretOf(t, s); ok {
		t.Fatal("a rejected token must not be persisted")
	}
}

// Reading the tab must never call Telegram. The poller holds the bot identity
// and the liveness signal, so a stored credential with no running poller
// reports itself as such instead of making an outbound request per page load.
func TestTelegramStatusNeverCallsTelegram(t *testing.T) {
	s := stubBotAPI(t, botAPI(t, 42, botToken, "piso_bot"))
	ch := s.ControlHandler()
	if w := doJSON(t, ch, "POST", "/api/v1/telegram", map[string]any{"token": botToken}); w.Code != 201 {
		t.Fatalf("connect %d %s", w.Code, w.Body.String())
	}
	s.HTTP = &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("status must not call Telegram, got %s", r.URL.Path)
	})}}

	body := doJSON(t, ch, "GET", "/api/v1/telegram", nil).Body.String()
	if !strings.Contains(body, `"connected":true`) || !strings.Contains(body, `"running":false`) {
		t.Fatalf("status %s", body)
	}
	if !strings.Contains(body, `"warning":"The Telegram poller has not started yet."`) {
		t.Fatalf("want a warning that the poller is not running, got %s", body)
	}
	if strings.Contains(body, botToken) {
		t.Fatalf("token leaked: %s", body)
	}
}

func TestTelegramReconnectReplacesTheTokenInPlace(t *testing.T) {
	const second = "987654321:BBHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"
	s := stubBotAPI(t, botAPI(t, 42, botToken, "piso_bot"))
	ch := s.ControlHandler()
	doJSON(t, ch, "POST", "/api/v1/telegram", map[string]any{"token": botToken})
	first, _ := telegramSecretOf(t, s)

	s.HTTP = &http.Client{Transport: handlerTransport{handler: botAPI(t, 42, second, "piso_bot_2")}}
	s.Telegram.HTTP = s.HTTP
	w := doJSON(t, ch, "POST", "/api/v1/telegram", map[string]any{"token": second})
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"botUsername":"piso_bot_2"`) {
		t.Fatalf("reconnect %d %s", w.Code, w.Body.String())
	}
	updated, ok := telegramSecretOf(t, s)
	if !ok || updated.Value != second {
		t.Fatalf("token not replaced: %+v", updated)
	}
	if updated.ID != first.ID {
		t.Fatalf("reconnect must reuse the secret id: %s -> %s", first.ID, updated.ID)
	}
	if all := s.Store.Secrets(); len(all) != 1 {
		t.Fatalf("want exactly one secret, got %d", len(all))
	}
}

func TestTelegramDisconnectIsIdempotent(t *testing.T) {
	s := stubBotAPI(t, botAPI(t, 42, botToken, "piso_bot"))
	if w := doJSON(t, s.ControlHandler(), "DELETE", "/api/v1/telegram", nil); w.Code != 204 {
		t.Fatalf("want 204 on a no-op disconnect, got %d %s", w.Code, w.Body.String())
	}
}

// A different bot is a different front door: its update stream and its people
// are not the previous bot's, so neither the cursor nor the allow-list carries
// over. A token rotation for the same bot keeps both.
func TestTelegramLinkIsKeyedByTheBot(t *testing.T) {
	const second = "987654321:BBHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"
	s := stubBotAPI(t, botAPI(t, 42, botToken, "piso_bot"))
	ch := s.ControlHandler()
	doJSON(t, ch, "POST", "/api/v1/telegram", map[string]any{"token": botToken})

	seed := func() {
		t.Helper()
		if err := s.Store.SetTelegramOffset(700); err != nil {
			t.Fatal(err)
		}
		if err := s.Store.UpsertTelegramUser(store.TelegramUser{UserID: 9, ChatID: 9, Username: "someone", ClaimedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	// Same bot, new token: the link survives, so a rotation is not a re-claim.
	seed()
	s.HTTP = &http.Client{Transport: handlerTransport{handler: botAPI(t, 42, second, "piso_bot")}}
	s.Telegram.HTTP = s.HTTP
	if w := doJSON(t, ch, "POST", "/api/v1/telegram", map[string]any{"token": second}); w.Code != 201 {
		t.Fatalf("rotate %d %s", w.Code, w.Body.String())
	}
	if link := s.Store.TelegramLink(); link.Offset != 700 || len(link.Users) != 1 {
		t.Fatalf("same bot must keep the link, got %+v", link)
	}

	// Different bot: both are reset. A stale cursor would skip the new bot's
	// updates and a stale allow-list would let strangers in.
	s.HTTP = &http.Client{Transport: handlerTransport{handler: botAPI(t, 77, botToken, "other_bot")}}
	s.Telegram.HTTP = s.HTTP
	if w := doJSON(t, ch, "POST", "/api/v1/telegram", map[string]any{"token": botToken}); w.Code != 201 {
		t.Fatalf("switch bot %d %s", w.Code, w.Body.String())
	}
	if link := s.Store.TelegramLink(); link.Offset != 0 || len(link.Users) != 0 || link.BotID != 77 {
		t.Fatalf("a different bot must reset the link, got %+v", link)
	}
}

func TestTelegramClaimCodeNeedsABot(t *testing.T) {
	s := stubBotAPI(t, botAPI(t, 42, botToken, "piso_bot"))
	ch := s.ControlHandler()
	if w := doJSON(t, ch, "POST", "/api/v1/telegram/claim", nil); w.Code != 409 {
		t.Fatalf("want 409 before a bot is connected, got %d %s", w.Code, w.Body.String())
	}
}

// The code is issued by the gateway and shown by the dashboard; it must appear
// in the status payload and must not be persisted.
func TestTelegramClaimCodeIsIssuedAndShown(t *testing.T) {
	s := stubBotAPI(t, botAPI(t, 42, botToken, "piso_bot"))
	ch := s.ControlHandler()
	doJSON(t, ch, "POST", "/api/v1/telegram", map[string]any{"token": botToken})

	var issued struct {
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	w := doJSON(t, ch, "POST", "/api/v1/telegram/claim", nil)
	if w.Code != 200 {
		t.Fatalf("claim %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if len(issued.Code) != 8 {
		t.Fatalf("unexpected code %q", issued.Code)
	}
	if !issued.ExpiresAt.After(time.Now()) {
		t.Fatalf("code already expired: %s", issued.ExpiresAt)
	}

	status := doJSON(t, ch, "GET", "/api/v1/telegram", nil).Body.String()
	if !strings.Contains(status, `"claimCode":"`+issued.Code+`"`) {
		t.Fatalf("status must show the pending code: %s", status)
	}
	// A code proves who is talking right now, so it must not outlive the
	// process that issued it.
	raw, err := os.ReadFile(s.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), issued.Code) {
		t.Fatalf("claim code was persisted: %s", raw)
	}
}

func TestTelegramRevoke(t *testing.T) {
	s := stubBotAPI(t, botAPI(t, 42, botToken, "piso_bot"))
	ch := s.ControlHandler()
	doJSON(t, ch, "POST", "/api/v1/telegram", map[string]any{"token": botToken})

	if err := s.Store.UpsertTelegramUser(store.TelegramUser{UserID: 9, ChatID: 9, Username: "someone", ClaimedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if body := doJSON(t, ch, "GET", "/api/v1/telegram", nil).Body.String(); !strings.Contains(body, `"userId":9`) {
		t.Fatalf("allow-list missing from status: %s", body)
	}

	if w := doJSON(t, ch, "DELETE", "/api/v1/telegram/users/9", nil); w.Code != 204 {
		t.Fatalf("revoke %d %s", w.Code, w.Body.String())
	}
	if body := doJSON(t, ch, "GET", "/api/v1/telegram", nil).Body.String(); strings.Contains(body, `"userId":9`) {
		t.Fatalf("revoked user still listed: %s", body)
	}
	if w := doJSON(t, ch, "DELETE", "/api/v1/telegram/users/9", nil); w.Code != 404 {
		t.Fatalf("want 404 for an unknown account, got %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(t, ch, "DELETE", "/api/v1/telegram/users/nope", nil); w.Code != 400 {
		t.Fatalf("want 400 for a bad id, got %d %s", w.Code, w.Body.String())
	}
}

// A gateway with the bot token but not the poller lease is connected without
// serving. The dashboard must say which, not show a generic "not started".
func TestTelegramStatusReportsAnotherGatewaysLease(t *testing.T) {
	s := stubBotAPI(t, botAPI(t, 42, botToken, "piso_bot"))
	ch := s.ControlHandler()
	doJSON(t, ch, "POST", "/api/v1/telegram", map[string]any{"token": botToken})

	// The lease is only asked for once the bot has been published, because
	// until then there is no coordinator resource to arbitrate.
	rec, ok := telegramSecretOf(t, s)
	if !ok {
		t.Fatal("no telegram secret stored")
	}
	rec.TechnocoreID = "11111111-1111-1111-1111-111111111111"
	if err := s.Store.ReplaceSecret(rec); err != nil {
		t.Fatal(err)
	}
	s.Telegram.Lease = func(context.Context, string, int64) (telegram.LeaseGrant, error) {
		return telegram.LeaseGrant{Granted: false, Holder: "other"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Telegram.Run(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body := doJSON(t, ch, "GET", "/api/v1/telegram", nil).Body.String()
		if strings.Contains(body, `"waiting":true`) {
			if !strings.Contains(body, "Another gateway holds the Telegram poller lease") {
				t.Fatalf("a waiting status must explain itself: %s", body)
			}
			if strings.Contains(body, `"leaseHeld":true`) {
				t.Fatalf("a waiting gateway must not claim the lease: %s", body)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for the lease wait to appear in the status")
}

func TestAttachCoordinatorInstallsTheClients(t *testing.T) {
	s := testServer(t)
	s.DataDir = t.TempDir()
	if s.Telegram.Lease != nil || s.Telegram.Remote != nil {
		t.Fatal("a server must start with no coordinator clients")
	}
	s.AttachCoordinator()
	if s.Telegram.Lease == nil || s.Telegram.Remote == nil {
		t.Fatal("AttachCoordinator did not install the clients")
	}
	// With no identity on disk the coordinator is unreachable, and the client
	// must report that rather than silently granting a lease.
	if _, err := s.Telegram.Lease(context.Background(), "rid", 0); err == nil {
		t.Fatal("want an error when the gateway is not enrolled")
	}
}

// The dashboard reads the allow-list through the front door, so a gateway whose
// coordinator is unreachable falls back to its cache with a warning instead of
// reporting that nobody is allowed.
func TestTelegramStatusFallsBackToTheCachedAllowList(t *testing.T) {
	s := stubBotAPI(t, botAPI(t, 42, botToken, "piso_bot"))
	ch := s.ControlHandler()
	doJSON(t, ch, "POST", "/api/v1/telegram", map[string]any{"token": botToken})

	rec, _ := telegramSecretOf(t, s)
	rec.TechnocoreID = "11111111-1111-1111-1111-111111111111"
	if err := s.Store.ReplaceSecret(rec); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetTelegramUsers([]store.TelegramUser{{UserID: 7, ChatID: 7, Username: "ada"}}); err != nil {
		t.Fatal(err)
	}
	// A remote front door with no identity on disk: every call fails.
	s.AttachCoordinator()

	body := doJSON(t, ch, "GET", "/api/v1/telegram", nil).Body.String()
	if !strings.Contains(body, `"userId":7`) {
		t.Fatalf("the cached allow-list must still be shown: %s", body)
	}
	if !strings.Contains(body, "cached allow-list") {
		t.Fatalf("a stale view must say so: %s", body)
	}
}
