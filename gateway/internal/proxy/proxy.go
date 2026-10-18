// Package proxy implements the gateway's MITM egress: explicit proxy (CONNECT
// + plain HTTP absolute-URI) with TLS termination, policy decision, credential
// substitution, forwarding, and blocked-request capture for replay.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"piso/gateway/internal/githubapp"
	"piso/gateway/internal/model"
	"piso/gateway/internal/patterns"
	"piso/gateway/internal/policy"
	"piso/gateway/internal/scanner"
	"piso/gateway/internal/store"
)

// headerTimeout is how long the forwarding client waits for origin response
// headers. LLM routers often hold headers until the model starts; 30s was
// producing empty 502s (http2: timeout awaiting response headers).
const headerTimeout = 90 * time.Second

// streamTimeout bounds a forwarded request including a streamed body.
// Completions can run for minutes after headers; the old 60s deadline
// canceled the origin body mid-stream.
const streamTimeout = 10 * time.Minute

// streamCopyBuf is the io.Copy buffer for tunnel bodies. The default 32KiB
// would hold SSE tokens in the gateway until the buffer filled.
const streamCopyBuf = 1024

// TokenMinter resolves a github-app placeholder to an installation token.
type TokenMinter interface {
	Mint(placeholder, host, path, body string) (string, error)
	LiveTokens() []string
}

// Handler is the egress MITM proxy.
type Handler struct {
	CA       *CA
	Patterns *patterns.Compiled
	Store    *store.Store
	Worker   func(*http.Request) string // request -> worker identity
	Minter   TokenMinter

	client *http.Client
}

// New builds the handler with a forwarding client that never follows
// redirects automatically: redirects are returned to the client so the next
// hop is re-scanned and re-policed by the gateway.
func New(ca *CA, st *store.Store, pat *patterns.Compiled, workerFn func(*http.Request) string) *Handler {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Never inherit HTTP_PROXY: the gateway is the proxy. DisableCompression
	// keeps upstream Content-Encoding intact so we do not rewrite lengths.
	// Keep HTTP/2 to origin (registry.npmjs.org ALPN is h2-only-enough that
	// ForceAttemptHTTP2=false reads SETTINGS as HTTP/1.1 and 502s). Rewrite
	// the response to HTTP/1.1 before writing it onto the MITM tunnel.
	tr.Proxy = nil
	tr.DisableCompression = true
	tr.ResponseHeaderTimeout = headerTimeout
	return &Handler{
		CA:       ca,
		Store:    st,
		Patterns: pat,
		Worker:   workerFn,
		Minter:   githubapp.NewPlaceholderMinter(st, githubapp.NewClient(nil)),
		client: &http.Client{
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// ServeHTTP dispatches CONNECT tunnels vs plain-HTTP absolute-URI requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		h.handleConnect(w, r)
		return
	}
	h.handlePlainHTTP(w, r)
}

// handleConnect establishes the MITM tunnel.
func (h *Handler) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if !strings.Contains(target, ":") {
		target += ":443"
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		http.Error(w, "bad CONNECT target", http.StatusBadRequest)
		return
	}
	// Hard network-level block on internal targets (belt & braces with policy).
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		h.Store.AppendLog(store.Record{
			ID: recID(), Worker: h.workerID(r), Slug: h.workerID(r), Ts: time.Now().UTC(),
			Method: http.MethodConnect, Scheme: "https", Host: host, Path: "/",
			Action: string(model.ActionBlock), Status: http.StatusForbidden,
			Reasons: []string{string(model.ReasonInternalTarget)},
		})
		http.Error(w, "internal target denied", http.StatusForbidden)
		return
	}
	// Internet kill-switch: worker toggled off in the dashboard can't egress.
	if blocked, slug := h.workerInternetBlocked(r); blocked {
		h.Store.AppendLog(store.Record{
			ID: recID(), Worker: h.workerID(r), Slug: slug, Ts: time.Now().UTC(),
			Method: http.MethodConnect, Scheme: "https", Host: host, Path: "/",
			Action: string(model.ActionBlock), Status: http.StatusForbidden,
			Reasons: []string{"internet-disabled"},
		})
		http.Error(w, "internet access disabled for this worker", http.StatusForbidden)
		return
	}

	// Resolve + verify the destination is not internal (hostnames may resolve
	// to private ranges — metadata-style attacks).
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		http.Error(w, "resolution failed", http.StatusBadGateway)
		return
	}
	for _, a := range addrs {
		if a.IP.IsLoopback() || a.IP.IsPrivate() || a.IP.IsLinkLocalUnicast() || a.IP.IsUnspecified() {
			http.Error(w, "internal target denied", http.StatusForbidden)
			return
		}
	}

	// Claim the tunnel.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		log.Printf("proxy: hijack failed: %v", err)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Time{})
	if _, err := brw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}

	// Server-side TLS with a leaf signed for this hostname.
	leaf, err := h.CA.Leaf(host)
	if err != nil {
		log.Printf("proxy: leaf for %s: %v", host, err)
		return
	}
	// The HTTP server's bufio.Reader may already hold the TLS ClientHello.
	// Handshake on the raw conn drops those bytes (bad record MAC / hang).
	tlsConn := tls.Server(&hijackedConn{Conn: conn, br: brw.Reader}, &tls.Config{
		Certificates: []tls.Certificate{leaf.toTLS()},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("proxy: tls handshake for %s: %v", host, err)
		return
	}
	defer tlsConn.Close()

	// Serve decrypted HTTP/1.1 from the client over the tunnel.
	// Do not use the CONNECT request context: it can be canceled after hijack.
	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return // EOF or client closed
		}
		req.URL.Scheme = "https"
		req.URL.Host = host
		if req.Host == "" {
			req.Host = host
		}
		// http.ReadRequest does not populate RemoteAddr (the server normally
		// does). The CONNECT request r carries the worker's origin IP — copy it
		// so worker-identity lookup (IP→slug) works for tunneled requests.
		req.RemoteAddr = r.RemoteAddr
		// streamTimeout covers headers + streamed body. headerTimeout on
		// the transport still fails fast when the origin never sends headers.
		fwdCtx, cancel := context.WithTimeout(context.Background(), streamTimeout)
		resp, keepAlive := h.process(fwdCtx, req, "https")
		writeErr := writeTunnelResponse(tlsConn, resp)
		_ = resp.Body.Close()
		cancel()
		if writeErr != nil {
			return
		}
		if resp.Close || !keepAlive {
			return
		}
	}
}

// hijackedConn reads leftover bytes from Hijack's bufio.Reader before the
// underlying connection. After CONNECT 200, the client often sends the TLS
// ClientHello before we call Handshake; those bytes sit in br, not conn.
type hijackedConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *hijackedConn) Read(p []byte) (int, error) {
	return c.br.Read(p)
}

// handlePlainHTTP forwards absolute-URI HTTP proxy requests.
func (h *Handler) handlePlainHTTP(w http.ResponseWriter, r *http.Request) {
	target, err := url.ParseRequestURI(r.RequestURI)
	if err != nil {
		http.Error(w, "bad request URI", http.StatusBadRequest)
		return
	}
	r.URL = target
	out, keepAlive := h.process(r.Context(), r, "http")
	defer out.Body.Close()
	if out.StatusCode == 0 {
		out.StatusCode = http.StatusOK
	}
	copyHeader(w.Header(), out.Header)
	w.WriteHeader(out.StatusCode)
	io.Copy(w, out.Body)
	if out.Close || !keepAlive {
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
			}
		}
	}
}

// Replay re-runs the full policy on a captured request and forwards it. This
// is the "add a secret, then retry" path — the policy is re-evaluated fresh;
// a rule added while the request sat blocked is never trusted blindly.
func (h *Handler) Replay(ctx context.Context, rec store.Record) (*http.Response, model.Decision, error) {
	if rec.Capture == nil {
		return nil, model.Decision{}, fmt.Errorf("no capture")
	}
	c := rec.Capture
	req, err := http.NewRequestWithContext(ctx, c.Method, c.URL, bytes.NewReader(c.Body))
	if err != nil {
		return nil, model.Decision{}, err
	}
	for k, vv := range c.Headers {
		if strings.EqualFold(k, "Host") {
			req.Host = vv[0]
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	body, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(body))

	dec := h.decide(req, rec.Scheme, body)
	if dec.Action != model.ActionAllow && dec.Action != model.ActionSubstitute {
		return nil, dec, fmt.Errorf("still blocked: %v", dec.Reasons)
	}
	if dec.Action == model.ActionSubstitute {
		if err := applySubstitution(req, body, dec, h.Store, scanner.ScanRequest(req, scanner.Options{
			Worker: h.workerID(req), Secrets: h.scanSecrets(), Patterns: h.Patterns,
		}), h.Minter); err != nil {
			return nil, model.Decision{Action: model.ActionBlock}, err
		}
	}
	out, err := h.forward(ctx, req, rec.Scheme)
	if err == nil && out != nil {
		// Same response backstop as process(). The replay body is discarded by
		// the control plane, but headers and any future reader must not see a
		// reflected real value.
		h.protectResponse(out, nil)
	}
	return out, dec, err
}

// decide is the scan + policy step shared by process and Replay.
func (h *Handler) decide(req *http.Request, scheme string, body []byte) model.Decision {
	scan := scanner.ScanRequest(req, scanner.Options{
		Worker: h.workerID(req), Secrets: h.scanSecrets(), Patterns: h.Patterns,
	})
	return policy.Decide(policy.Input{
		Method: req.Method, Schema: scheme, Host: req.URL.Hostname(), Path: req.URL.Path,
		Body: string(body), Scan: scan,
		SecretByPlaceholder:    placeholderMap(h.Store),
		Rules:                  recRules(h.Store),
		Domains:                recDomains(h.Store),
		Exceptions:             recExceptions(h.Store),
		Worker:                 h.workerID(req),
		GithubAppByPlaceholder: h.Store.GithubAppViews(),
	})
}

// process is the core: scan → decide → substitute/block → forward → record.
func (h *Handler) process(ctx context.Context, req *http.Request, scheme string) (*http.Response, bool) {
	body, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(body))

	// Internet kill-switch: worker toggled off can't egress (plain HTTP too).
	if blocked, slug := h.workerInternetBlocked(req); blocked {
		reqID := newID()
		rec := store.Record{
			ID: recID(), Worker: h.workerID(req), Slug: slug, Ts: time.Now().UTC(),
			Method: req.Method, Scheme: scheme, Host: req.URL.Hostname(),
			Path: req.URL.RequestURI(), RequestID: reqID,
			Action: string(model.ActionBlock), Status: http.StatusForbidden,
			Reasons: []string{"internet-disabled"},
		}
		h.Store.AppendLog(rec)
		return &http.Response{
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			StatusCode: http.StatusForbidden, Status: "403 Forbidden",
			Header:  http.Header{"Connection": {"close"}},
			Body:    io.NopCloser(bytes.NewReader(nil)),
			Request: req,
			Close:   true,
		}, false
	}

	dec := h.decide(req, scheme, body)
	scan := scanner.ScanRequest(req, scanner.Options{
		Worker: h.workerID(req), Secrets: h.scanSecrets(), Patterns: h.Patterns,
	})

	reqID := newID()
	wid := h.workerID(req)
	rec := store.Record{
		ID:        recID(),
		Worker:    wid,
		Slug:      wid,
		Ts:        time.Now().UTC(),
		Method:    req.Method,
		Scheme:    scheme,
		Host:      req.URL.Hostname(),
		Path:      req.URL.RequestURI(),
		RequestID: reqID,
	}
	rec.Findings = toStoreFindings(scan)

	switch dec.Action {
	case model.ActionBlock:
		rec.Action = string(model.ActionBlock)
		rec.Status = http.StatusProxyAuthRequired
		rec.Reasons = toStrings(dec.Reasons)
		rec.Retryable = true
		rec.Capture = capture(req, body)
		h.Store.AppendLog(rec)
		resp := &http.Response{
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			StatusCode: http.StatusProxyAuthRequired,
			Status:     "407 Proxy Authentication Required",
			Header: http.Header{
				"X-Piso-Request-Id": {reqID},
				"X-Piso-Reason":     {string(dec.Reasons[0])},
				"Content-Length":    {"0"},
				"Connection":        {"close"},
			},
			Body:          io.NopCloser(bytes.NewReader(nil)),
			ContentLength: 0,
			Request:       req,
			Close:         true,
		}
		return resp, false

	case model.ActionSubstitute:
		if err := applySubstitution(req, body, dec, h.Store, scan, h.Minter); err != nil {
			reason := "credential-resolution-failed"
			if errors.Is(err, errUnsupportedSite) {
				// A rule exists, but the placeholder sits in a body shape the
				// gateway refuses to blind-replace. Retrying cannot help until
				// the caller moves it to a header/credential field, so this is
				// not a retryable 407.
				reason = string(model.ReasonUnsupportedSite)
			}
			status := http.StatusBadGateway
			rec.Action, rec.Status = string(model.ActionBlock), status
			rec.Reasons = []string{reason}
			rec.Retryable = false
			h.Store.AppendLog(rec)
			return &http.Response{StatusCode: status, Status: http.StatusText(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"X-Piso-Reason": {reason}}, Body: io.NopCloser(bytes.NewReader(nil)), Request: req, Close: true}, false
		}
		rec.Action = string(model.ActionSubstitute)
		rec.Status = http.StatusOK
		rec.Reasons = toStrings(dec.Reasons)

	case model.ActionAllow:
		rec.Action = string(model.ActionAllow)
		rec.Status = http.StatusOK
		rec.Reasons = toStrings(dec.Reasons)
	}

	out, err := h.forward(ctx, req, scheme)
	if err != nil {
		log.Printf("proxy: forward to %s failed: %v", req.URL.Host, err)
		rec.Action = string(model.ActionBlock)
		rec.Status = http.StatusBadGateway
		rec.Reasons = append(rec.Reasons, "upstream-error")
		h.Store.AppendLog(rec)
		return &http.Response{
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			StatusCode: http.StatusBadGateway,
			Status:     "502 Bad Gateway",
			Header:     http.Header{"Connection": {"close"}},
			Body:       io.NopCloser(bytes.NewReader(nil)),
			Request:    req,
			Close:      true,
		}, false
	}
	prepareTunnelResponse(out)
	out.Request = req
	rec.Retryable = false
	h.Store.AppendLog(rec)

	// Response backstop: replace known real secrets that an origin reflects
	// back (model echo, issue body, error page) with their placeholders. This
	// is detection/mitigation for anything the request-site allowlist missed.
	h.protectResponse(out, func(location, secretID string) {
		h.Store.AppendLog(store.Record{
			ID: recID(), Worker: wid, Slug: wid, Ts: time.Now().UTC(),
			Method: req.Method, Scheme: scheme, Host: req.URL.Hostname(),
			Path: req.URL.RequestURI(), Action: string(model.ActionSubstitute),
			Status: out.StatusCode, RequestID: reqID,
			Reasons: []string{string(model.ReasonResponseSecretLeak)},
			Findings: []store.Finding{{
				Kind: string(model.FindingRealSecret), SecretID: secretID,
				Token: "[redacted]", Location: location, Count: 1,
			}},
		})
	})
	return out, true
}

// prepareTunnelResponse rewrites an upstream response so it is valid HTTP/1.1
// on the MITM tunnel. HTTP/2 origins set ProtoMajor=2; Alt-Svc tells the
// client to switch to h2/h3 on the next request — both break this proxy.
func prepareTunnelResponse(resp *http.Response) {
	if resp == nil {
		return
	}
	resp.Proto = "HTTP/1.1"
	resp.ProtoMajor = 1
	resp.ProtoMinor = 1
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	for _, h := range []string{
		"Alt-Svc",
		"Alt-Used",
		"HTTP2-Settings",
		"Upgrade",
		"Keep-Alive",
		"Proxy-Connection",
	} {
		resp.Header.Del(h)
	}
}

// writeTunnelResponse writes a clean HTTP/1.1 response onto the MITM tunnel.
// Origin bodies are streamed with chunked encoding so chat completions are
// not held until EOF (buffering hid the first token and raced streamTimeout).
// HTTP/2 Content-Length is ignored: origins often advertise the wrong length.
// The caller still closes the body.
func writeTunnelResponse(w io.Writer, resp *http.Response) error {
	if resp == nil {
		return fmt.Errorf("nil response")
	}
	prepareTunnelResponse(resp)

	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	text := http.StatusText(status)
	if text == "" {
		text = "OK"
	}

	hdr := make(http.Header)
	if resp.Header != nil {
		hdr = resp.Header.Clone()
	}
	for _, h := range []string{"Content-Length", "Transfer-Encoding"} {
		hdr.Del(h)
	}

	noBody := !responseHasBody(resp)
	if noBody {
		hdr.Set("Content-Length", "0")
	} else {
		hdr.Set("Transfer-Encoding", "chunked")
	}

	if _, err := fmt.Fprintf(w, "HTTP/1.1 %03d %s\r\n", status, text); err != nil {
		return err
	}
	if err := hdr.Write(w); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "\r\n"); err != nil {
		return err
	}
	flushWriter(w)
	if noBody {
		return nil
	}

	cw := httputil.NewChunkedWriter(w)
	if _, err := io.CopyBuffer(cw, resp.Body, make([]byte, streamCopyBuf)); err != nil {
		_ = cw.Close()
		return err
	}
	if err := cw.Close(); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\r\n")
	if err != nil {
		return err
	}
	flushWriter(w)
	return nil
}

// responseHasBody reports whether the response carries an entity body on the
// wire. Content-Length is ignored: HTTP/2 origins often advertise 0 or a
// stale value while still sending bytes.
func responseHasBody(resp *http.Response) bool {
	if resp == nil || resp.Body == nil {
		return false
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return false
	}
	if resp.Request != nil && resp.Request.Method == http.MethodHead {
		return false
	}
	return true
}

// flushWriter flushes w when it exposes Flush, so SSE tokens leave the
// gateway instead of sitting in a bufio buffer.
func flushWriter(w io.Writer) {
	switch f := w.(type) {
	case interface{ Flush() error }:
		_ = f.Flush()
	case interface{ Flush() }:
		f.Flush()
	}
}

// forward replays the (possibly substituted) request upstream. The body is
// already in memory (process read it for scanning), so GetBody is set and a
// dead HTTP/2 connection can be retried once — Go will not retry POST itself.
func (h *Handler) forward(ctx context.Context, req *http.Request, scheme string) (*http.Response, error) {
	target := *req.URL
	target.Scheme = scheme
	target.Host = req.URL.Host
	outReq := req.Clone(ctx)
	outReq.URL = &target
	outReq.RequestURI = ""
	// The response redactor scans for known secrets as plain text. A gzip/br
	// body would hide every match, so ask origins for identity encoding. The
	// tunnel already replaces Content-Length with chunked framing.
	outReq.Header.Set("Accept-Encoding", "identity")
	if err := ensureGetBody(outReq); err != nil {
		return nil, err
	}

	resp, err := h.client.Do(outReq)
	if err == nil || !isRetryableUpstream(err) || outReq.GetBody == nil {
		return resp, err
	}

	body, getErr := outReq.GetBody()
	if getErr != nil {
		return nil, err
	}
	retry := outReq.Clone(ctx)
	retry.Body = body
	retry.GetBody = outReq.GetBody
	log.Printf("proxy: retrying %s %s after dead upstream conn: %v", retry.Method, retry.URL.Host, err)
	return h.client.Do(retry)
}

// ensureGetBody snapshots req.Body so the transport (and forward's own retry)
// can rewind POST/PUT/PATCH after a dead connection. No-op when GetBody is
// already set or there is no body.
func ensureGetBody(req *http.Request) error {
	if req == nil {
		return fmt.Errorf("nil request")
	}
	if req.GetBody != nil {
		return nil
	}
	if req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	b, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return err
	}
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	req.Body = io.NopCloser(bytes.NewReader(b))
	req.ContentLength = int64(len(b))
	return nil
}

// isRetryableUpstream reports whether err is a dead/idle HTTP/2 (or TCP)
// connection, safe to retry once because the body is rewindable. Timeouts
// and cancellations are not retried: those are a slow or abandoned origin.
func isRetryableUpstream(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"http2: server sent goaway",
		"http2: client connection lost",
		"http2: client conn not usable",
		"http2: transport received server's graceful shutdown goaway",
		"refused stream",
		"connection reset by peer",
		"broken pipe",
		"use of closed network connection",
		"server closed idle connection",
		"http2: connection error",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// applySubstitution replaces placeholder tokens with real values in headers
// and non-transcript body fields. The LLM messages[] transcript is never
// rewritten: a whole-body ReplaceAll would leak the real secret to the model
// whenever chat text mentioned the same piso_ token (including the provider
// key sitting in Authorization). Pass the original body bytes from process.
func applySubstitution(req *http.Request, origBody []byte, dec model.Decision, st *store.Store, scan scanner.Result, mint TokenMinter) error {
	pm := placeholderMap(st)
	if len(dec.Substituted) == 0 {
		return nil
	}
	host := ""
	path := ""
	if req.URL != nil {
		host = req.URL.Hostname()
		path = req.URL.Path
	}
	body := string(origBody)
	sub := map[string]string{}
	for _, tok := range dec.Substituted {
		s, ok := pm[tok]
		if !ok {
			return fmt.Errorf("secret unavailable")
		}
		if s.Kind == model.SecretKindGithubApp {
			if mint == nil {
				return fmt.Errorf("github token minter unavailable")
			}
			real, err := mint.Mint(tok, host, path, body)
			if err != nil || real == "" {
				return fmt.Errorf("github token mint failed")
			}
			sub[tok] = real
			continue
		}
		if s.Value != "" {
			sub[tok] = s.Value
		}
	}
	if len(sub) == 0 {
		return nil
	}
	for name, vals := range req.Header {
		if !scanner.HeaderIsCredentialSite(name) {
			continue // User-Agent, Referer, X-Title, Accept… are payload, not credentials
		}
		auth := http.CanonicalHeaderKey(name) == "Authorization"
		for i, v := range vals {
			nv := v
			if auth {
				// Authorization: Basic base64-wraps the payload, so a literal
				// ReplaceAll on the raw value never reaches the placeholder.
				// Decode, substitute inside the payload, re-encode.
				if payload, ok := scanner.BasicAuthPayload(v); ok {
					nd := payload
					for tok, real := range sub {
						nd = strings.ReplaceAll(nd, tok, real)
					}
					if nd != payload {
						vals[i] = "Basic " + base64.StdEncoding.EncodeToString([]byte(nd))
						continue
					}
				}
			}
			for tok, real := range sub {
				nv = strings.ReplaceAll(nv, tok, real)
			}
			vals[i] = nv
		}
	}
	// URL path and query. Telegram-style APIs put the credential in the path
	// (https://api.telegram.org/bot<token>/getMe). Rewrite the decoded path and
	// clear RawPath so EscapedPath re-encodes it — the real token may contain
	// ':' which stays literal in a path segment. Query is rewritten on RawQuery
	// with the real value escaped, so the original query order/encoding is kept
	// (re-encoding via url.Values would reorder and can break signed queries).
	// Only credential-named query parameters are touched: a placeholder in
	// ?q= or a tracking parameter is payload, not a credential.
	if req.URL != nil {
		if p := req.URL.Path; p != "" {
			np := p
			for tok, real := range sub {
				np = strings.ReplaceAll(np, tok, real)
			}
			if np != p {
				req.URL.Path = np
				req.URL.RawPath = ""
			}
		}
		if q := req.URL.RawQuery; q != "" {
			req.URL.RawQuery = rewriteEncodedPairs(q, url.QueryEscape, sub)
		}
	}
	if len(origBody) == 0 {
		return nil
	}
	if !containsAnyToken(origBody, sub) {
		return nil // headers/query/path only: leave the body untouched
	}
	var nb []byte
	switch {
	case looksJSON(origBody):
		rewritten, ok := substituteJSONSkippingChat(origBody, sub)
		if !ok {
			return errUnsupportedSite
		}
		nb = rewritten
	case isFormBody(req):
		nb = substituteForm(origBody, sub)
	default:
		// Never blind-replace an opaque body. A placeholder here could be a
		// model prompt, an issue body, or a gist: substituting publishes the
		// real credential. Block instead (the caller maps this to
		// unsupported-substitution-site).
		return errUnsupportedSite
	}
	req.Body = io.NopCloser(bytes.NewReader(nb))
	req.ContentLength = int64(len(nb))
	req.Header.Set("Content-Length", fmt.Sprint(len(nb)))
	return nil
}

// looksJSON reports whether the body starts like one JSON object/array.
func looksJSON(body []byte) bool {
	t := bytes.TrimSpace(body)
	return len(t) > 0 && (t[0] == '{' || t[0] == '[')
}

// isFormBody reports whether the request declares an urlencoded form body.
func isFormBody(req *http.Request) bool {
	ct := req.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/x-www-form-urlencoded")
}

// ---- helpers ----

func knownSecrets(st *store.Store) []scanner.KnownSecret {
	recs := st.Secrets()
	out := make([]scanner.KnownSecret, 0, len(recs))
	for _, r := range recs {
		if r.Value != "" {
			out = append(out, scanner.KnownSecret{ID: r.ID, Value: r.Value})
		}
	}
	return out
}

func (h *Handler) scanSecrets() []scanner.KnownSecret {
	out := knownSecrets(h.Store)
	for _, app := range h.Store.GithubApps() {
		if app.PrivateKey != "" {
			out = append(out, scanner.KnownSecret{ID: "gha-pem-" + app.ID, Value: app.PrivateKey})
		}
	}
	if h.Minter != nil {
		for i, tok := range h.Minter.LiveTokens() {
			out = append(out, scanner.KnownSecret{ID: fmt.Sprintf("ghs-%d", i), Value: tok})
		}
	}
	return out
}

func placeholderMap(st *store.Store) map[string]model.Secret {
	recs := st.Secrets()
	out := make(map[string]model.Secret, len(recs))
	for _, r := range recs {
		if r.Placeholder != "" {
			out[r.Placeholder] = model.Secret{
				ID: r.ID, Name: r.Name, Placeholder: r.Placeholder, Value: r.Value, Kind: r.Kind,
				Workers: r.Workers, AllowedHosts: r.AllowedHosts, GatewayOnly: r.GatewayOnly,
			}
		}
	}
	return out
}

func recRules(st *store.Store) []model.Rule {
	rs := st.Rules()
	out := make([]model.Rule, 0, len(rs))
	for _, r := range rs {
		out = append(out, model.Rule{ID: r.ID, SecretID: r.SecretID, Host: r.Host, Placeholder: r.Placeholder, Note: r.Note})
	}
	return out
}

func recDomains(st *store.Store) map[string]model.DomainPolicy {
	ds := st.Domains()
	out := make(map[string]model.DomainPolicy, len(ds))
	for _, d := range ds {
		out[strings.ToLower(d.Host)] = model.DomainPolicy{Host: d.Host, Deny: d.Deny, NeedsRule: d.NeedsRule, Note: d.Note}
	}
	return out
}

func recExceptions(st *store.Store) []model.Exception {
	es := st.Exceptions()
	out := make([]model.Exception, 0, len(es))
	for _, e := range es {
		out = append(out, model.Exception{
			ID: e.ID, Note: e.Note, HostRegex: e.HostRegex, PathRegex: e.PathRegex,
			ContentRegex: e.ContentRegex, Placeholder: e.Placeholder,
			PatternID: e.PatternID, Enabled: e.Enabled,
		})
	}
	return out
}

// toStoreFindings normalizes scanner findings for the request log.
// Transcript-only noise is dropped (see findingIsChatNoise); what remains is
// collapsed per (kind, pattern, secret, token, location). The first occurrence
// keeps its field and a Count of 1; later ones bump Count. The dedupe key uses
// the FULL token, so two distinct secrets sharing a name prefix never merge.
func toStoreFindings(scan scanner.Result) []store.Finding {
	var out []store.Finding
	index := make(map[string]int, len(scan.Placeholders)+len(scan.PatternHits)+len(scan.RealSecrets))
	add := func(f model.Finding) {
		if findingIsChatNoise(f) {
			return
		}
		key := strings.Join([]string{
			string(f.Kind), f.PatternID, f.SecretID, f.Token, f.Location,
		}, "\x00")
		if i, ok := index[key]; ok {
			out[i].Count++
			return
		}
		index[key] = len(out)
		out = append(out, store.Finding{
			Kind: string(f.Kind), PatternID: f.PatternID, SecretID: f.SecretID,
			Token: displayToken(f.Kind, f.Token), Location: f.Location, Field: f.Field, Count: 1,
		})
	}
	for _, f := range scan.RealSecrets {
		add(f)
	}
	for _, f := range scan.PatternHits {
		add(f)
	}
	for _, f := range scan.Placeholders {
		add(f)
	}
	return out
}

// findingIsChatNoise reports whether a finding sits in model payload content
// (messages[], system, instructions, input, tool descriptions, …). The gateway
// never substitutes placeholders there and never blocks on pattern hits there
// (policy.egressPatternHits), so logging them just floods Traffic with
// conversation — often strings that merely look like placeholders (shell
// function names such as piso_gateway_post, docs, smoke-test literals). A
// real-secret finding is kept: it blocks even when it appears in the payload.
func findingIsChatNoise(f model.Finding) bool {
	if !scanner.IsPayloadField(f.Location, f.Field) {
		return false
	}
	return f.Kind == model.FindingPlaceholder || f.Kind == model.FindingPatternSecret
}

// displayToken renders a finding token for the log. A placeholder is not a
// secret — the worker holds it, the gateway substitutes it, and the captured
// request already stores it whole — and its trailing random id is the only
// thing that tells two similarly-named secrets apart. Truncating it to a name
// prefix made distinct findings render identically, so placeholders are kept
// intact (middle-truncated only if absurdly long). Real-secret and pattern
// tokens are already redacted by the scanner and stay bounded here.
func displayToken(kind model.FindingKind, tok string) string {
	if tok == "" {
		return ""
	}
	if kind == model.FindingPlaceholder {
		if len(tok) <= 48 {
			return tok
		}
		if i := strings.LastIndex(tok, "_"); i > 5 && i < len(tok)-1 {
			return tok[:i+1] + "…" + tok[len(tok)-4:]
		}
		return tok[:24] + "…" + tok[len(tok)-4:]
	}
	if len(tok) > 24 {
		return tok[:12] + "…"
	}
	return tok
}

func capture(req *http.Request, body []byte) *store.Capture {
	hdr := make(map[string][]string, len(req.Header))
	for k, v := range req.Header {
		hdr[k] = append([]string(nil), v...)
	}
	return &store.Capture{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: hdr,
		Body:    append([]byte(nil), body...),
	}
}

func (h *Handler) workerID(r *http.Request) string {
	if h.Worker != nil {
		if w := h.Worker(r); w != "" {
			return w
		}
	}
	return "worker"
}

// workerInternetBlocked reports whether the request's origin worker has the
// internet kill-switch enabled. It resolves the origin IP via the proxy's
// worker function (identity), then checks the registry flag. identity empty
// → not blocked (unknown worker can't be toggled).
func (h *Handler) workerInternetBlocked(r *http.Request) (blocked bool, slug string) {
	if h.Worker == nil {
		return false, ""
	}
	identity := h.workerID(r)
	if identity == "" || identity == "worker" {
		return false, ""
	}
	// workerID returns the slug when known; look up by that slug's name is
	// ambiguous — instead resolve by origin IP to get the full registry entry.
	// The proxy's worker fn returns the slug; find the record by it after all
	// (Slug is unique per worker).
	if rec, ok := h.Store.WorkerBySlug(identity); ok {
		return rec.InternetDisabled, rec.Slug
	}
	return false, ""
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	dst.Del("Proxy-Authenticate")
	dst.Del("Proxy-Authorization")
}

func toStrings(rs []model.Reason) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, string(r))
	}
	return out
}
