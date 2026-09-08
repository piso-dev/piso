package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWriteTunnelResponseForcesHTTP11(t *testing.T) {
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		ProtoMinor:    0,
		Header:        http.Header{"Alt-Svc": {`h3=":443"`}, "Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(`{"ok":true}`)),
		ContentLength: 11,
	}
	var buf bytes.Buffer
	if err := writeTunnelResponse(&buf, resp); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "HTTP/1.1 200") {
		t.Fatalf("want HTTP/1.1 status line, got %q", got[:min(48, len(got))])
	}
	if strings.Contains(got, "HTTP/2") {
		t.Fatalf("HTTP/2 leaked onto the tunnel:\n%s", got)
	}
	if strings.Contains(strings.ToLower(got), "alt-svc") {
		t.Fatalf("Alt-Svc leaked onto the tunnel:\n%s", got)
	}
	if !strings.Contains(got, `{"ok":true}`) {
		t.Fatalf("body missing:\n%s", got)
	}
}

func TestWriteTunnelResponseDropsUntrustedContentLength(t *testing.T) {
	body := []byte(`{"ok":true}`)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		ProtoMajor:    2,
		Header:        http.Header{"Content-Length": {"9999"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: 9999,
	}
	var buf bytes.Buffer
	if err := writeTunnelResponse(&buf, resp); err != nil {
		t.Fatal(err)
	}
	raw := buf.String()
	if strings.Contains(raw, "Content-Length: 9999") {
		t.Fatalf("untrusted HTTP/2 Content-Length leaked:\n%s", raw)
	}

	parsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf.Bytes())), nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	got, err := io.ReadAll(parsed.Body)
	_ = parsed.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("body %q, want %q", got, body)
	}
}

func TestWriteTunnelResponseStreamsBeforeEOF(t *testing.T) {
	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		Header:        http.Header{"Content-Type": {"text/event-stream"}},
		Body:          pr,
		ContentLength: -1,
	}

	outR, outW := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		errCh <- writeTunnelResponse(outW, resp)
		_ = outW.Close()
	}()

	parsed, err := http.ReadResponse(bufio.NewReader(outR), nil)
	if err != nil {
		t.Fatalf("headers should arrive before origin EOF: %v", err)
	}
	if parsed.StatusCode != http.StatusOK {
		t.Fatalf("status %d", parsed.StatusCode)
	}

	first := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, readErr := parsed.Body.Read(buf)
		if n > 0 {
			first <- string(buf[:n])
			return
		}
		if readErr != nil {
			first <- ""
		}
	}()

	if _, err := pw.Write([]byte("data: token\n\n")); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-first:
		if !strings.Contains(got, "data: token") {
			t.Fatalf("streamed body %q, want token", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not see body bytes until origin EOF (still buffering)")
	}

	_ = pw.Close()
	_ = parsed.Body.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("writeTunnelResponse: %v", err)
	}
}
