// Command relay splices remote workers to standalone gateways that have no
// public URL. Gateways dial out (POST /tunnel/register). Workers POST
// /gw/<principal-id>/in for CONNECT tunnels and /gw/<id>/api/... for the
// worker API. The coordinator never terminates inner TLS or sees vault plaintext.
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	addr := envOr("PISO_RELAY_LISTEN", ":8085")
	verify := envOr("PISO_VERIFY_GATEWAY_URL", "http://server:8000/api/v1/internal/verify-gateway")
	r := &relay{verifyURL: verify, idle: map[string]chan net.Conn{}}
	log.Printf("relay listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatal(err)
	}
}

type relay struct {
	verifyURL string
	mu        sync.Mutex
	idle      map[string]chan net.Conn
}

func (r *relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == "/tunnel/register" && req.Method == http.MethodPost:
		r.handleRegister(w, req)
	case strings.HasPrefix(req.URL.Path, "/gw/"):
		r.handleGW(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (r *relay) handleGW(w http.ResponseWriter, req *http.Request) {
	rest := strings.TrimPrefix(req.URL.Path, "/gw/")
	i := strings.IndexByte(rest, '/')
	if i <= 0 {
		http.NotFound(w, req)
		return
	}
	id, tail := rest[:i], rest[i:]
	if tail == "/in" {
		r.splice(w, req, id, true)
		return
	}
	req.URL.Path = tail
	req.URL.RawPath = tail
	req.RequestURI = tail
	r.splice(w, req, id, false)
}

func (r *relay) handleRegister(w http.ResponseWriter, req *http.Request) {
	id, err := r.verifyGateway(req)
	if err != nil || id == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}
	_, _ = bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: keep-alive\r\n\r\n")
	_ = bufrw.Flush()
	ch := r.pool(id)
	select {
	case ch <- prefixConn{Conn: conn, r: bufrw.Reader}:
	case <-time.After(5 * time.Minute):
		conn.Close()
	}
}

func (r *relay) splice(w http.ResponseWriter, req *http.Request, id string, preludeDone bool) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	var gw net.Conn
	select {
	case gw = <-r.pool(id):
	case <-time.After(20 * time.Second):
		http.Error(w, "gateway unavailable", http.StatusBadGateway)
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		gw.Close()
		return
	}
	if preludeDone {
		_, _ = bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		_ = bufrw.Flush()
		pipe(prefixConn{Conn: conn, r: bufrw.Reader}, gw)
		return
	}
	if err := req.Write(gw); err != nil {
		gw.Close()
		conn.Close()
		return
	}
	pipe(prefixConn{Conn: conn, r: bufrw.Reader}, gw)
}

func (r *relay) pool(id string) chan net.Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.idle[id]
	if !ok {
		ch = make(chan net.Conn, 8)
		r.idle[id] = ch
	}
	return ch
}

func (r *relay) verifyGateway(req *http.Request) (string, error) {
	vr, err := http.NewRequest(http.MethodGet, r.verifyURL, nil)
	if err != nil {
		return "", err
	}
	for _, h := range []string{"X-Piso-Pubkey", "X-Piso-Timestamp", "X-Piso-Nonce", "X-Piso-Signature"} {
		vr.Header.Set(h, req.Header.Get(h))
	}
	vr.Header.Set("X-Piso-Auth-Method", req.Method)
	vr.Header.Set("X-Piso-Auth-Path", req.URL.Path)
	res, err := http.DefaultClient.Do(vr)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", io.EOF
	}
	var out struct {
		PrincipalID string `json:"principal_id"`
	}
	if err := json.Unmarshal(rawOrEmpty(res.Body), &out); err != nil {
		return "", err
	}
	return out.PrincipalID, nil
}

func rawOrEmpty(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, 1<<16))
	return b
}

type prefixConn struct {
	net.Conn
	r *bufio.Reader
}

func (c prefixConn) Read(p []byte) (int, error) {
	if c.r != nil && c.r.Buffered() > 0 {
		return c.r.Read(p)
	}
	return c.Conn.Read(p)
}

func pipe(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
