package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
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

func TestWriteTunnelResponseFixesContentLength(t *testing.T) {
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
	got := buf.String()
	if !strings.Contains(got, "Content-Length: 11\r\n") {
		t.Fatalf("want Content-Length of actual body, got:\n%s", got)
	}
}
