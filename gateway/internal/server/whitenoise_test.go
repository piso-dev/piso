package server

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"piso/gateway/internal/store"
	"piso/gateway/internal/whitenoise"
	"testing"
	"time"
)

func wnFixture(t *testing.T, s *Server) {
	t.Helper()
	if err := s.Store.SetWhiteNoiseLink(store.WhiteNoiseLink{ResourceID: "wn-connection", AccountID: "agent", Enabled: true, Users: []store.WhiteNoiseUser{{PublicKey: "person"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetWhiteNoiseSupervisor(store.SupervisorLink{Worker: mailWorker, PrincipalID: "principal", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.EnqueueWhiteNoise(store.WhiteNoiseMail{ID: "wn-message", ConnectionID: "wn-connection", ConversationID: "opaque-group", SenderID: "person", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
}
func TestWhiteNoiseMailboxChannelIsolation(t *testing.T) {
	s := testServer(t)
	wnFixture(t, s)
	w := doJSON(t, s.WorkerHandler(), "GET", "/api/v1/worker/whitenoise/messages?worker="+mailWorker+"&wait=1", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var out struct {
		Messages []store.WhiteNoiseMail `json:"messages"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Messages) != 1 || out.Messages[0].Channel != "whitenoise" || out.Messages[0].ConversationID != "opaque-group" {
		t.Fatalf("%s", w.Body.String())
	}
	ack := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/messages/ack", map[string]any{"worker": mailWorker, "ids": []string{"wn-message"}})
	if ack.Code != 200 || len(s.Store.WhiteNoiseLink().Mailbox) != 1 {
		t.Fatal("Telegram ack crossed channels")
	}
	ack = doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/whitenoise/messages/ack", map[string]any{"worker": mailWorker, "ids": []string{"wn-message"}})
	if ack.Code != 200 || len(s.Store.WhiteNoiseLink().Mailbox) != 0 {
		t.Fatalf("ack %d %s", ack.Code, ack.Body.String())
	}
}
func TestWhiteNoiseSocketToMailboxAndReply(t *testing.T) {
	s := testServer(t)
	wnFixture(t, s)
	path := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sent := make(chan map[string]any, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var req map[string]any
				if json.NewDecoder(c).Decode(&req) != nil {
					return
				}
				out := map[string]any{"marmot_agent_control": whitenoise.Protocol, "id": req["id"]}
				switch req["type"] {
				case "subscribe_inbound":
					out["type"] = "ack"
					json.NewEncoder(c).Encode(out)
					out["type"] = "inbound_message"
					out["account_id_hex"] = "agent"
					out["group_id_hex"] = "opaque-group"
					out["message"] = map[string]any{"message_id_hex": "socket-message", "text": "from phone", "sender": map[string]any{"account_id_hex": "person"}}
				case "group_info":
					out["type"] = "group_info"
					out["is_direct"] = true
				case "send_final":
					sent <- req
					out["type"] = "final_sent"
					out["message_ids_hex"] = []string{"encrypted-send"}
				default:
					return
				}
				json.NewEncoder(c).Encode(out)
			}(c)
		}
	}()
	client := &whitenoise.Client{Socket: path}
	s.whiteNoise.client = client
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()
	_ = client.Subscribe(ctx, "agent", func(e whitenoise.Event) error { return s.acceptWhiteNoise(ctx, e) })
	setLiveSupervisor(t, s, mailWorker)
	telegram := doJSON(t, s.WorkerHandler(), "GET", "/api/v1/worker/messages?worker="+mailWorker+"&wait=1", nil)
	if telegram.Code != 200 || len(s.Store.Mail()) != 0 {
		t.Fatal("White Noise leaked into Telegram mailbox")
	}
	w := doJSON(t, s.WorkerHandler(), "GET", "/api/v1/worker/whitenoise/messages?worker="+mailWorker+"&wait=1", nil)
	var out struct {
		Messages []store.WhiteNoiseMail `json:"messages"`
	}
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Messages) != 2 || out.Messages[1].Text != "from phone" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	reply := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/whitenoise/reply", map[string]any{"worker": mailWorker, "connectionId": "wn-connection", "conversationId": "opaque-group", "text": "from supervisor", "idempotencyKey": "turn-1"})
	if reply.Code != 200 {
		t.Fatalf("reply %d %s", reply.Code, reply.Body.String())
	}
	select {
	case req := <-sent:
		if req["text"] != "from supervisor" || req["group_id_hex"] != "opaque-group" || req["idempotency_key"] != "wn-connection:opaque-group:turn-1" {
			t.Fatalf("send %+v", req)
		}
	case <-ctx.Done():
		t.Fatal("no send on connector")
	}
	if err := s.Store.SetWhiteNoiseUsers(nil); err != nil {
		t.Fatal(err)
	}
	denied := doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/whitenoise/reply", map[string]any{"worker": mailWorker, "connectionId": "wn-connection", "conversationId": "opaque-group", "text": "after revoke", "idempotencyKey": "turn-2"})
	if denied.Code != 403 {
		t.Fatalf("revoke %d %s", denied.Code, denied.Body.String())
	}
}

func TestWhiteNoiseMailboxUnauthorized(t *testing.T) {
	s := testServer(t)
	wnFixture(t, s)
	for _, worker := range []string{"piso-worker-other", "piso-worker-telegram"} {
		w := doJSON(t, s.WorkerHandler(), "GET", "/api/v1/worker/whitenoise/messages?worker="+worker, nil)
		if w.Code != 409 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	if err := s.Store.SetWhiteNoiseUsers(nil); err != nil {
		t.Fatal(err)
	}
	if len(s.Store.WhiteNoiseLink().Mailbox) != 0 || s.Store.WhiteNoiseDestination("wn-connection", "opaque-group") {
		t.Fatal("unlink retained access")
	}
}
