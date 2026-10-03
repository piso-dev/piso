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

// RunTunnel keeps reverse-tunnel connections to the coordinator so remote
// workers can reach this standalone gateway without a public URL.
func RunTunnel(id tc.Identity, dataDir string, handler http.Handler) {
	if handler == nil || strings.TrimSpace(id.ServerURL) == "" {
		return
	}
	for {
		if err := dialTunnelOnce(id, handler); err != nil {
			log.Printf("technocore: tunnel: %v", err)
		}
		time.Sleep(2 * time.Second)
		if live, err := tc.Load(tc.Path(dataDir)); err == nil {
			id = live
			if id.PublicURL != "" || id.PlacementOrLocal() != tc.PlacementStandalone {
				log.Printf("technocore: tunnel: stopped (direct URL or not standalone)")
				return
			}
		}
	}
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
