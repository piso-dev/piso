package technocore

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

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

func connectSession(id tc.Identity, dataDir string, nudge func(), onEvent func(string)) (ready bool, err error) {
	if nudge == nil {
		nudge = func() {}
	}
	if onEvent == nil {
		onEvent = func(string) {}
	}
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
	if err := conn.WriteJSON(heartbeatFields(id, dataDir, map[string]string{
		"type":      "auth",
		"pubkey":    strings.ToLower(id.SigningPublicKey),
		"timestamp": ts,
		"nonce":     nonce,
		"signature": sig,
	})); err != nil {
		return false, err
	}

	done := make(chan struct{})
	defer close(done)
	path := tc.Path(dataDir)
	sendHeartbeat := func() {
		live := id
		if cur, err := tc.Load(path); err == nil {
			live = cur
		}
		_ = conn.WriteJSON(heartbeatFields(live, dataDir, map[string]string{"type": "heartbeat"}))
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				sendHeartbeat()
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
			// HTTP reconcile runs on its own goroutine. Do not block this read loop.
			nudge()
		case "envelopes_changed", "gateways_changed", "workers_changed":
			nudge()
		case "asks_changed":
			onEvent("asks_changed")
		case "pong", "ack", "hello":
		case "error":
			return ready, fmt.Errorf("server error: %v", msg["error"])
		default:
			raw, _ := json.Marshal(msg)
			log.Printf("technocore: ws %s", raw)
		}
	}
}

func heartbeatFields(id tc.Identity, dataDir string, extra map[string]string) map[string]string {
	out := map[string]string{
		"placement": id.PlacementOrLocal(),
		"publicUrl": strings.TrimRight(id.PublicURL, "/"),
	}
	for k, v := range extra {
		out[k] = v
	}
	if raw, err := os.ReadFile(filepath.Join(dataDir, "ca.crt")); err == nil {
		out["caPem"] = string(raw)
	}
	return out
}
