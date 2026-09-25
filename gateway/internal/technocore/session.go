package technocore

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

func httpToWS(serverURL string) string {
	u := strings.TrimRight(serverURL, "/")
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	default:
		return "wss://" + u
	}
}

func originFor(serverURL string) string {
	u := strings.TrimRight(serverURL, "/")
	switch {
	case strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"):
		return u
	default:
		return "https://" + u
	}
}

func connectSession(st *store.Store, id tc.Identity) (ready bool, err error) {
	wsURL := httpToWS(id.ServerURL) + "/ws/gateway/"
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second}
	header := http.Header{}
	header.Set("Origin", originFor(id.ServerURL))
	conn, resp, err := dialer.Dial(wsURL, header)
	if err != nil {
		if resp != nil {
			return false, fmt.Errorf("websocket dial: %w (HTTP %d)", err, resp.StatusCode)
		}
		return false, fmt.Errorf("websocket dial: %w", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	var hello map[string]any
	if err := conn.ReadJSON(&hello); err != nil {
		return false, fmt.Errorf("websocket hello: %w", err)
	}

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce, err := tc.NewNonce()
	if err != nil {
		return false, err
	}
	sig, err := tc.SignAuth(id, ts, tc.WSMethod, tc.WSPath, tc.BodyHashHex(nil), nonce)
	if err != nil {
		return false, err
	}
	if err := conn.WriteJSON(map[string]string{
		"type":      "auth",
		"pubkey":    strings.ToLower(id.SigningPublicKey),
		"timestamp": ts,
		"nonce":     nonce,
		"signature": sig,
	}); err != nil {
		return false, err
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_ = conn.WriteJSON(map[string]string{"type": "ping"})
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			return ready, err
		}
		switch fmt.Sprint(msg["type"]) {
		case "ready":
			ready = true
			log.Printf("technocore: websocket ready")
			if err := syncSecrets(st, id); err != nil {
				log.Printf("technocore: sync: %v", err)
			}
		case "envelopes_changed":
			if err := syncSecrets(st, id); err != nil {
				log.Printf("technocore: sync: %v", err)
			}
		case "pong", "ack", "hello":
		case "error":
			return ready, fmt.Errorf("server error: %v", msg["error"])
		default:
			raw, _ := json.Marshal(msg)
			log.Printf("technocore: ws %s", raw)
		}
	}
}
