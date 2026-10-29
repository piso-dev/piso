package proxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A plain-HTTP proxied response must arrive with its body.
//
// handlePlainHTTP writes the body through net/http's buffered writer and then
// hijacks the connection to close it. Hijacking takes the connection away from
// that writer, so the response is only complete if the buffer is flushed first;
// without it the tail — in practice every body smaller than the 4KB buffer —
// is discarded together with the connection. The CONNECT path does not have
// this hazard because writeTunnelResponse writes straight onto the socket.
func TestPlainHTTPProxyDeliversBody(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An origin that closes the connection is what reaches the hijack
		// branch; a keep-alive origin is flushed by net/http on return.
		w.Header().Set("Connection", "close")
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "small-body")
	}))
	defer origin.Close()

	h, _ := testHandler(t)
	h.client = &http.Client{Transport: &http.Transport{}}
	front := httptest.NewServer(h)
	defer front.Close()
	proxyURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Address the origin as localhost rather than 127.0.0.1: the loopback IP
	// literal is an internal target the policy refuses, and a worker reaching
	// a provider beside it uses a name, not the literal.
	_, port, err := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := client.Get("http://localhost:" + port + "/")
	if err != nil {
		t.Fatalf("plain-http proxied response: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := string(body); got != "small-body" {
		t.Fatalf("body = %q, want %q", got, "small-body")
	}
}
