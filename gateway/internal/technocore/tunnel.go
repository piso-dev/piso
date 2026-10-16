package technocore

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	tc "piso/internal/technocore"
)

const tunnelPoolSize = 8

// RelayNeeded reports whether remote workers still depend on the coordinator
// relay. The gateway process injects this so the pool stays up after a
// public URL is set until every assigned worker has checked in on it.
type RelayNeeded func() bool

// RunTunnel keeps a pool of reverse-tunnel connections to the coordinator so
// remote workers can reach this standalone gateway without a public URL.
// One registration carries one worker request. The pool redials as slots
// free. It stops only when the gateway is no longer standalone, or when a
// public URL is set and need reports that every remote worker has switched.
func RunTunnel(id tc.Identity, dataDir string, handler http.Handler, need RelayNeeded) {
	if handler == nil || strings.TrimSpace(id.ServerURL) == "" {
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < tunnelPoolSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tunnelSlot(id, dataDir, handler, need)
		}()
	}
	wg.Wait()
}

func tunnelSlot(id tc.Identity, dataDir string, handler http.Handler, need RelayNeeded) {
	backoff := time.Second
	for {
		live, stop := tunnelShouldStop(id, dataDir, need)
		if stop {
			log.Printf("technocore: tunnel: stopped (direct URL and workers switched, or not standalone)")
			return
		}
		id = live
		err := dialTunnelOnce(id, handler)
		if err != nil {
			log.Printf("technocore: tunnel: %v", err)
			time.Sleep(backoff)
			if backoff < 15*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func tunnelShouldStop(id tc.Identity, dataDir string, need RelayNeeded) (tc.Identity, bool) {
	if live, err := tc.Load(tc.Path(dataDir)); err == nil {
		id = live
	}
	if id.PlacementOrLocal() != tc.PlacementStandalone || id.Status != tc.StatusActive {
		return id, true
	}
	if strings.TrimSpace(id.PublicURL) == "" {
		return id, false
	}
	if need == nil {
		return id, false
	}
	return id, !need()
}

func dialTunnelOnce(id tc.Identity, handler http.Handler) error {
	raw := strings.TrimRight(id.ServerURL, "/") + "/tunnel/register"
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	if err := tc.SignRequest(req, id, nil); err != nil {
		return err
	}

	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	d := net.Dialer{Timeout: 20 * time.Second}
	addr := net.JoinHostPort(u.Hostname(), port)
	var conn net.Conn
	if u.Scheme == "http" {
		conn, err = d.Dial("tcp", addr)
	} else {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{
			ServerName: u.Hostname(),
			MinVersion: tls.VersionTLS12,
		})
	}
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}

	if err := req.Write(conn); err != nil {
		conn.Close()
		return err
	}
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return fmt.Errorf("relay response: %w", err)
	}
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		res.Body.Close()
		conn.Close()
		return fmt.Errorf("relay HTTP %d: %s", res.StatusCode, body)
	}
	// Leftover buffered bytes belong to the spliced worker session.
	served := &prefixConn{Conn: conn, prefix: br}
	ln := &oneConnListener{c: served}
	return http.Serve(ln, handler)
}

type prefixConn struct {
	net.Conn
	prefix *bufio.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if c.prefix != nil && c.prefix.Buffered() > 0 {
		return c.prefix.Read(p)
	}
	return c.Conn.Read(p)
}

type oneConnListener struct {
	mu   sync.Mutex
	c    net.Conn
	done bool
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done || l.c == nil {
		return nil, io.EOF
	}
	l.done = true
	c := l.c
	l.c = nil
	return c, nil
}

func (l *oneConnListener) Close() error {
	if l.c != nil {
		return l.c.Close()
	}
	return nil
}

func (l *oneConnListener) Addr() net.Addr {
	if l.c != nil {
		return l.c.LocalAddr()
	}
	return &net.TCPAddr{}
}
