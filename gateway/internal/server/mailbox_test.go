package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
	"piso/gateway/internal/telegram"
)

const mailWorker = "piso-worker-ship"

func setLiveSupervisor(t *testing.T, s *Server, worker string) {
	t.Helper()
	if err := s.Store.SetSupervisorLink(store.SupervisorLink{
		Worker: worker, PrincipalID: "prin-" + worker,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
}

// Without a supervisor the poller must not accept-and-drop: Handle reports no
// recipient so the message gets the no-worker acknowledgement instead.
func TestMailboxRefusesWithNoSupervisor(t *testing.T) {
	s := testServer(t)
	s.attachTelegramMailbox()
	err := s.Telegram.Handle(context.Background(), telegram.Message{
		Chat: telegram.Chat{ID: 9}, From: &telegram.User{ID: 9}, Text: "hi",
	})
	if !errors.Is(err, telegram.ErrNoRecipient) {
		t.Fatalf("err %v", err)
	}
	if len(s.Store.Mail()) != 0 {
		t.Fatal("must not enqueue without a supervisor")
	}
}

func TestMailboxEnqueuesForTheSupervisor(t *testing.T) {
	s := testServer(t)
	s.attachTelegramMailbox()
	setLiveSupervisor(t, s, mailWorker)
	err := s.Telegram.Handle(context.Background(), telegram.Message{
		Chat: telegram.Chat{ID: 9}, From: &telegram.User{ID: 9}, Text: "  hello  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	mail := s.Store.Mail()
	if len(mail) != 1 || mail[0].ChatID != 9 || mail[0].Text != "hello" || mail[0].State != store.MailPending {
		t.Fatalf("mailbox %+v", mail)
	}
}

func TestMessagesRequireTheSupervisor(t *testing.T) {
	s := testServer(t)
	w := doJSON(t, s.WorkerHandler(), "GET", "/api/v1/worker/messages?worker="+mailWorker+"&wait=1", nil)
	if w.Code != 409 {
		t.Fatalf("want 409, got %d %s", w.Code, w.Body.String())
	}
}

// The long-poll must wake as soon as a message lands, not wait out its window.
func TestMessagesLongPollWakesOnEnqueue(t *testing.T) {
	s := testServer(t)
	setLiveSupervisor(t, s, mailWorker)
	req := httptest.NewRequest("GET", "/api/v1/worker/messages?worker="+mailWorker+"&wait=5", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.WorkerHandler().ServeHTTP(rec, req)
	}()
	time.Sleep(50 * time.Millisecond)
	s.attachTelegramMailbox()
	if err := s.Telegram.Handle(context.Background(), telegram.Message{
		Chat: telegram.Chat{ID: 9}, From: &telegram.User{ID: 9}, Text: "wake up",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("long poll did not wake on enqueue")
	}
	var out struct {
		Messages []store.MailMessage `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Text != "wake up" {
		t.Fatalf("messages %+v", out.Messages)
	}
}

func TestMessagesTimeoutReturnsEmpty(t *testing.T) {
	s := testServer(t)
	setLiveSupervisor(t, s, mailWorker)
	w := doJSON(t, s.WorkerHandler(), "GET", "/api/v1/worker/messages?worker="+mailWorker+"&wait=1", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"messages":[]`) {
		t.Fatalf("timeout %d %s", w.Code, w.Body.String())
	}
}

func TestAckRemovesDeliveredMessages(t *testing.T) {
	s := testServer(t)
	setLiveSupervisor(t, s, mailWorker)
	if _, err := s.Store.EnqueueMail(store.MailMessage{ChatID: 9, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.WorkerHandler(), "GET", "/api/v1/worker/messages?worker="+mailWorker+"&wait=1", nil)
	var out struct {
		Messages []store.MailMessage `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("take %s", w.Body.String())
	}
	ack := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/messages/ack",
		map[string]any{"worker": mailWorker, "ids": []string{out.Messages[0].ID}})
	if ack.Code != 200 {
		t.Fatalf("ack %d %s", ack.Code, ack.Body.String())
	}
	if len(s.Store.Mail()) != 0 {
		t.Fatalf("mailbox not empty: %+v", s.Store.Mail())
	}
}

func TestReplySendsOnlyToAllowedChats(t *testing.T) {
	s := testServer(t)
	setLiveSupervisor(t, s, mailWorker)
	if err := s.Store.AddSecret(store.SecretRec{
		ID: "sec_bot", Name: "Telegram bot", Kind: model.SecretKindTelegram,
		GatewayOnly: true, Value: botToken, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpsertTelegramUser(store.TelegramUser{UserID: 9, ChatID: 9, ClaimedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var sentChat string
	s.Telegram.HTTP = &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sentChat = r.Form.Get("chat_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":9,"type":"private"},"text":"x"}}`))
	})}}

	ok := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/reply",
		map[string]any{"worker": mailWorker, "chatId": 9, "text": "answer"})
	if ok.Code != 200 {
		t.Fatalf("reply %d %s", ok.Code, ok.Body.String())
	}
	if sentChat != "9" {
		t.Fatalf("sent to chat %q", sentChat)
	}
	// A chat that never claimed the bot is not a megaphone.
	blocked := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/reply",
		map[string]any{"worker": mailWorker, "chatId": 10, "text": "spam"})
	if blocked.Code != 403 {
		t.Fatalf("want 403, got %d %s", blocked.Code, blocked.Body.String())
	}
}

func TestReplyRequiresTheSupervisor(t *testing.T) {
	s := testServer(t)
	if err := s.Store.UpsertTelegramUser(store.TelegramUser{UserID: 9, ChatID: 9, ClaimedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/reply",
		map[string]any{"worker": mailWorker, "chatId": 9, "text": "answer"})
	if w.Code != 409 {
		t.Fatalf("want 409, got %d %s", w.Code, w.Body.String())
	}
}

// A status line is one message that follows the turn, so the second write must
// edit the first one rather than post another.
func TestStatusEditsTheSameMessage(t *testing.T) {
	s := testServer(t)
	setLiveSupervisor(t, s, mailWorker)
	if err := s.Store.AddSecret(store.SecretRec{
		ID: "sec_bot", Name: "Telegram bot", Kind: model.SecretKindTelegram,
		GatewayOnly: true, Value: botToken, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpsertTelegramUser(store.TelegramUser{UserID: 9, ChatID: 9, ClaimedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	type call struct{ method, text, messageID string }
	var calls []call
	s.Telegram.HTTP = &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		calls = append(calls, call{method, r.Form.Get("text"), r.Form.Get("message_id")})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":55,"date":0,"chat":{"id":9,"type":"private"},"text":"x"}}`))
	})}}

	first := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/status",
		map[string]any{"worker": mailWorker, "chatId": 9, "text": "🔧 bash go test"})
	if first.Code != 200 || !strings.Contains(first.Body.String(), `"messageId":55`) {
		t.Fatalf("status %d %s", first.Code, first.Body.String())
	}
	second := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/status",
		map[string]any{"worker": mailWorker, "chatId": 9, "text": "✅ done", "messageId": 55})
	if second.Code != 200 {
		t.Fatalf("status %d %s", second.Code, second.Body.String())
	}
	if len(calls) != 2 || calls[0].method != "sendMessage" || calls[1].method != "editMessageText" {
		t.Fatalf("calls %+v", calls)
	}
	if calls[1].messageID != "55" || calls[1].text != "✅ done" {
		t.Fatalf("edit %+v", calls[1])
	}
	// Status lines are still the bot speaking: an unclaimed chat is refused.
	blocked := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/status",
		map[string]any{"worker": mailWorker, "chatId": 10, "text": "spam"})
	if blocked.Code != 403 {
		t.Fatalf("want 403, got %d %s", blocked.Code, blocked.Body.String())
	}
}

// A status line from a worker that is not the live supervisor is refused for
// the same reason a reply is: the bot belongs to the gateway, not the worker.
func TestStatusRequiresTheSupervisor(t *testing.T) {
	s := testServer(t)
	if err := s.Store.UpsertTelegramUser(store.TelegramUser{UserID: 9, ChatID: 9, ClaimedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/status",
		map[string]any{"worker": mailWorker, "chatId": 9, "text": "working"})
	if w.Code != 409 {
		t.Fatalf("want 409, got %d %s", w.Code, w.Body.String())
	}
}
