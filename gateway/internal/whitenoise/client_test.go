package whitenoise

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentControlRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, _ := ln.Accept()
		defer c.Close()
		var req map[string]any
		json.NewDecoder(c).Decode(&req)
		if req["marmot_agent_control"] != Protocol || req["type"] != "subscribe_inbound" {
			return
		}
		json.NewEncoder(c).Encode(map[string]any{"marmot_agent_control": Protocol, "id": req["id"], "type": "inbound_message", "account_id_hex": strings.Repeat("a", 64), "group_id_hex": "1234", "message": map[string]any{"message_id_hex": strings.Repeat("b", 64), "sender": map[string]any{"account_id_hex": strings.Repeat("c", 64)}, "text": "hello"}})
	}()
	client := Client{Socket: path}
	got := false
	err = client.Subscribe(context.Background(), strings.Repeat("a", 64), func(e Event) error { got = e.Message.Text == "hello"; return nil })
	if !got {
		t.Fatalf("inbound not routed: %v", err)
	}
}

func TestClientChecksCorrelationAndBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock")
	ln, _ := net.Listen("unix", path)
	defer ln.Close()
	go func() {
		c, _ := ln.Accept()
		defer c.Close()
		bufio.NewReader(c).ReadString('\n')
		json.NewEncoder(c).Encode(map[string]any{"marmot_agent_control": Protocol, "id": "wrong", "type": "ack"})
	}()
	_, err := (&Client{Socket: path}).Call(context.Background(), map[string]any{"type": "account_list"})
	if err == nil {
		t.Fatal("accepted mismatched correlation")
	}
}

func TestPublicKeyOnly(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	npub, err := PublicKey(hex)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParsePublicKey(npub)
	if err != nil || got != hex {
		t.Fatalf("roundtrip %s %v", got, err)
	}
	for _, bad := range []string{"nsec1secret", "npub1bad", strings.Repeat("a", 64), "", npub[:len(npub)-1] + "q"} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
