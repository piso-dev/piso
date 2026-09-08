package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// roundTripFunc is an http.RoundTripper adapter for tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestNewUsesHeaderTimeout(t *testing.T) {
	h := New(nil, nil, nil, nil)
	tr, ok := h.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("client transport is not *http.Transport")
	}
	if tr.ResponseHeaderTimeout != headerTimeout {
		t.Fatalf("ResponseHeaderTimeout %v, want %v", tr.ResponseHeaderTimeout, headerTimeout)
	}
	if headerTimeout < 90*time.Second {
		t.Fatalf("headerTimeout %v is still in the range that 502'd routstr", headerTimeout)
	}
	if streamTimeout < 5*time.Minute {
		t.Fatalf("streamTimeout %v is too short for streamed completions", streamTimeout)
	}
}

func TestIsRetryableUpstream(t *testing.T) {
	timeout := timeoutErr{}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "deadline", err: context.DeadlineExceeded, want: false},
		{name: "wrapped deadline", err: fmt.Errorf("Post: %w", context.DeadlineExceeded), want: false},
		{name: "canceled", err: context.Canceled, want: false},
		{name: "net timeout", err: timeout, want: false},
		{name: "header timeout string", err: errors.New("http2: timeout awaiting response headers"), want: false},
		{name: "eof", err: io.EOF, want: true},
		{name: "wrapped eof", err: fmt.Errorf("Post https://x: %w", io.EOF), want: true},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, want: true},
		{name: "goaway", err: errors.New("http2: server sent GOAWAY and closed the connection; LastStreamID=3"), want: true},
		{name: "conn lost", err: errors.New("http2: client connection lost"), want: true},
		{name: "reset", err: errors.New("read tcp: connection reset by peer"), want: true},
		{name: "idle", err: errors.New("net/http: server closed idle connection"), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableUpstream(tc.err); got != tc.want {
				t.Fatalf("isRetryableUpstream(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestForwardRetriesDeadHTTP2Conn(t *testing.T) {
	var attempts atomic.Int32
	h := &Handler{
		client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				b, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				if string(b) != `{"m":1}` {
					return nil, fmt.Errorf("body %q", b)
				}
				n := attempts.Add(1)
				if n == 1 {
					return nil, errors.New("http2: server sent GOAWAY and closed the connection")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("ok")),
					Request:    req,
				}, nil
			}),
		},
	}
	req, err := http.NewRequest(http.MethodPost, "https://routstr.ft.hn/v1/chat/completions", strings.NewReader(`{"m":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.forward(context.Background(), req, "https")
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts %d, want 2 (one retry)", got)
	}
}

func TestForwardDoesNotRetryHeaderTimeout(t *testing.T) {
	var attempts atomic.Int32
	h := &Handler{
		client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Body != nil {
					_, _ = io.ReadAll(req.Body)
					_ = req.Body.Close()
				}
				attempts.Add(1)
				return nil, timeoutErr{}
			}),
		},
	}
	req, err := http.NewRequest(http.MethodPost, "https://routstr.ft.hn/v1/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.forward(context.Background(), req, "https")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts %d, want 1 (no retry on timeout)", got)
	}
}

// timeoutErr implements net.Error with Timeout() true.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "http2: timeout awaiting response headers" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
