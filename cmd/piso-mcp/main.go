// Command piso-mcp is the worker-side relay for the encrypted gateway MCP
// channel. Pi talks to it over stdio. Shell helpers use `piso-mcp call`.
// The process never logs request or response content.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"piso/internal/mcpchannel"
	"piso/internal/technocore"
)

const (
	defaultTimeout = 60 * time.Second
	identityPath   = "/root/.pi/agent/piso/identity.json"
	mountedIdent   = "/etc/piso/identity.json"
	pinPath        = "/etc/piso/gateway.json"
	caPath         = "/piso-ca.pem"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "piso-mcp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return serveStdio(os.Stdin, os.Stdout, os.Stderr)
	}
	switch args[0] {
	case "identity":
		return cmdIdentity(args[1:])
	case "key":
		return cmdKey()
	case "call":
		return cmdCall(args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(os.Stderr, "usage: piso-mcp [identity ensure|show | key | call <method> [json]]")
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdIdentity(args []string) error {
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "ensure":
		id, err := ensureIdentity()
		if err != nil {
			return err
		}
		fmt.Println(id.SigningPublicKey)
		return nil
	case "show":
		id, err := technocore.Load(workerIdentityPath())
		if err != nil {
			return err
		}
		fmt.Printf("file:        %s\n", workerIdentityPath())
		fmt.Printf("public:      %s\n", id.SigningPublicKey)
		fmt.Printf("fingerprint: %s\n", id.Fingerprint())
		return nil
	default:
		return fmt.Errorf("identity %s", sub)
	}
}

func cmdKey() error {
	pin, err := mcpchannel.LoadPin(pinPath)
	if err != nil {
		return fmt.Errorf("pinned gateway key: %w", err)
	}
	fmt.Printf("file:   %s\n", pinPath)
	fmt.Printf("keyId:  %s\n", pin.KeyID)
	fmt.Printf("x25519: %s\n", pin.X25519)
	return nil
}

func cmdCall(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: piso-mcp call <method> [json]")
	}
	method := args[0]
	var params json.RawMessage
	if len(args) > 1 {
		params = json.RawMessage(args[1])
		if !json.Valid(params) {
			return errors.New("call argument is not json")
		}
	}
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if len(params) > 0 {
		req["params"] = json.RawMessage(params)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	resp, err := c.roundTrip(context.Background(), raw)
	if err != nil {
		return err
	}
	os.Stdout.Write(resp)
	if len(resp) == 0 || resp[len(resp)-1] != '\n' {
		os.Stdout.Write([]byte("\n"))
	}
	return nil
}

// ensureIdentity loads the mounted remote identity, or generates one on the
// agent volume. A corrupt file is an error; it is never overwritten.
func ensureIdentity() (technocore.Identity, error) {
	if raw, err := os.ReadFile(mountedIdent); err == nil {
		id, err := technocore.ParseIdentity(raw)
		if err != nil {
			return technocore.Identity{}, fmt.Errorf("%s: %w", mountedIdent, err)
		}
		if err := os.MkdirAll("/root/.pi/agent/piso", 0o700); err != nil {
			return technocore.Identity{}, err
		}
		if err := technocore.Save(identityPath, id); err != nil {
			return technocore.Identity{}, err
		}
		return id, nil
	} else if !os.IsNotExist(err) {
		return technocore.Identity{}, err
	}
	return technocore.LoadOrCreate(identityPath, "", "worker")
}

func workerIdentityPath() string {
	if p := os.Getenv("PISO_MCP_IDENTITY"); p != "" {
		return p
	}
	return identityPath
}

func serveStdio(in io.Reader, out io.Writer, errOut io.Writer) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	var mu sync.Mutex
	write := func(line []byte) {
		mu.Lock()
		defer mu.Unlock()
		out.Write(line)
		if len(line) == 0 || line[len(line)-1] != '\n' {
			out.Write([]byte("\n"))
		}
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), mcpchannel.MaxRequest)
	var wg sync.WaitGroup
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if isBatch(line) {
			write(localError(nil, "batches are not supported"))
			continue
		}
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			write(localError(nil, "invalid json"))
			continue
		}
		note := len(probe.ID) == 0 || string(probe.ID) == "null"
		wg.Add(1)
		go func(line []byte, id json.RawMessage, note bool) {
			defer wg.Done()
			resp, err := c.roundTrip(context.Background(), line)
			if note {
				return
			}
			if err != nil {
				write(localError(id, err.Error()))
				return
			}
			write(resp)
		}(line, append(json.RawMessage(nil), probe.ID...), note)
	}
	wg.Wait()
	if err := sc.Err(); err != nil {
		fmt.Fprintf(errOut, "piso-mcp: stdin: %v\n", err)
		return err
	}
	return nil
}

type client struct {
	http     *http.Client
	url      string
	token    string
	pin      mcpchannel.Pin
	identity technocore.Identity
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
}

func newClient() (*client, error) {
	pin, err := mcpchannel.LoadPin(envOr("PISO_MCP_PIN", pinPath))
	if err != nil {
		return nil, fmt.Errorf("gateway pin missing (%s): %w", pinPath, err)
	}
	id, err := technocore.Load(workerIdentityPath())
	if err != nil {
		return nil, fmt.Errorf("worker identity: %w", err)
	}
	priv, err := id.SigningPrivate()
	if err != nil {
		return nil, err
	}
	pub := priv.Public().(ed25519.PublicKey)
	gw := strings.TrimRight(envOr("GATEWAY_URL", "http://gateway:8083"), "/")
	timeout := defaultTimeout
	if v := os.Getenv("PISO_MCP_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			timeout = d
		}
	}
	tr := &http.Transport{Proxy: nil}
	if strings.HasPrefix(gw, "https://") {
		pool, err := caPool()
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &client{
		http:     &http.Client{Timeout: timeout, Transport: tr},
		url:      gw + "/mcp",
		token:    os.Getenv("PISO_WORKER_TOKEN"),
		pin:      pin,
		identity: id,
		priv:     priv,
		pub:      pub,
	}, nil
}

func (c *client) roundTrip(ctx context.Context, content []byte) ([]byte, error) {
	gwPub, err := c.pin.X25519Bytes()
	if err != nil {
		return nil, err
	}
	sealed, respCtx, err := mcpchannel.SealRequest(gwPub, c.pub, c.priv, content, time.Now())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(sealed))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mcpchannel.ContentType)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("gateway unreachable")
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, mcpchannel.MaxResponse+1))
	if err != nil {
		return nil, errors.New("gateway response")
	}
	if res.StatusCode == http.StatusAccepted {
		return nil, nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway status %d", res.StatusCode)
	}
	if len(body) > mcpchannel.MaxResponse {
		return nil, errors.New("gateway response too large")
	}
	pt, err := respCtx.OpenResponse(body)
	if err != nil {
		return nil, fmt.Errorf("gateway key mismatch (pinned %s)", c.pin.KeyID)
	}
	return pt, nil
}

func caPool() (*x509.CertPool, error) {
	raw, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, errors.New("ca: no certificates")
	}
	return pool, nil
}

func localError(id json.RawMessage, msg string) []byte {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32000, "message": msg},
	}
	if len(id) == 0 {
		resp["id"] = nil
	}
	b, _ := json.Marshal(resp)
	return b
}

func isBatch(raw []byte) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r':
			continue
		case '[':
			return true
		default:
			return false
		}
	}
	return false
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
