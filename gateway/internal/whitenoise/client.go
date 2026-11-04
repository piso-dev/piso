// Package whitenoise adapts the gateway-only wn-agent Unix control socket.
// It never handles MLS keys and exposes no connector administration to workers.
package whitenoise

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"time"
)

const Protocol = "marmot.agent-control.v2"
const MaxFrame = 1024 * 1024

var ErrUnavailable = errors.New("White Noise connector is unavailable")

type Client struct{ Socket string }
type Event struct {
	Type    string `json:"type"`
	Account string `json:"account_id_hex"`
	Group   string `json:"group_id_hex"`
	Message struct {
		ID       string `json:"message_id_hex"`
		Text     string `json:"text"`
		Recorded uint64 `json:"recorded_at"`
		Sender   struct {
			ID   string `json:"account_id_hex"`
			Self bool   `json:"is_self"`
		} `json:"sender"`
	} `json:"message"`
}

func requestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (c *Client) open(ctx context.Context, req map[string]any) (net.Conn, *bufio.Scanner, string, error) {
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, nil, "", ErrUnavailable
	}
	id := requestID()
	req["id"] = id
	req["marmot_agent_control"] = Protocol
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err = json.NewEncoder(conn).Encode(req); err != nil {
		conn.Close()
		return nil, nil, "", ErrUnavailable
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), MaxFrame)
	return conn, scanner, id, nil
}
func frame(scanner *bufio.Scanner, id string) (map[string]any, error) {
	if !scanner.Scan() {
		return nil, ErrUnavailable
	}
	var out map[string]any
	if json.Unmarshal(scanner.Bytes(), &out) != nil || out["marmot_agent_control"] != Protocol || out["id"] != id || out["type"] == "error" {
		return nil, ErrUnavailable
	}
	return out, nil
}
func (c *Client) Call(ctx context.Context, req map[string]any) (map[string]any, error) {
	conn, scanner, id, err := c.open(ctx, req)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	return frame(scanner, id)
}
func (c *Client) Subscribe(ctx context.Context, account string, handle func(Event) error) error {
	conn, scanner, id, err := c.open(ctx, map[string]any{"type": "subscribe_inbound", "account_id_hex": account, "group_id_hex": nil})
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	for {
		out, err := frame(scanner, id)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(out)
		var event Event
		if json.Unmarshal(raw, &event) != nil {
			return ErrUnavailable
		}
		if err = handle(event); err != nil {
			return err
		}
	}
}
