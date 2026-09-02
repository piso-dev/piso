// Package proxy implements the gateway's MITM egress: explicit proxy (CONNECT
// + plain HTTP absolute-URI) with TLS termination, policy decision, credential
// substitution, forwarding, and blocked-request capture for replay.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"piso/gateway/internal/model"
	"piso/gateway/internal/patterns"
	"piso/gateway/internal/policy"
	"piso/gateway/internal/scanner"
	"piso/gateway/internal/store"
)

// Handler is the egress MITM proxy.
type Handler struct {
	CA       *CA
	Patterns *patterns.Compiled
	Store    *store.Store
	Worker   func(*http.Request) string // request -> worker identity

	client *http.Client
}

// New builds the handler with a forwarding client that never follows
// redirects automatically: redirects are returned to the client so the next
// hop is re-scanned and re-policed by the gateway.
func New(ca *CA, st *store.Store, pat *patterns.Compiled, workerFn func(*http.Request) string) *Handler {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	return &Handler{
		CA:       ca,
		Store:    st,
		Patterns: pat,
		Worker:   workerFn,
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
			ID: recID(), Worker: h.workerID(r), Ts: time.Now().UTC(),
			Method: http.MethodConnect, Scheme: "https", Host: host, Path: "/",
			Action: string(model.ActionBlock), Status: http.StatusForbidden,
			Reasons: []string{string(model.ReasonInternalTarget)},
		})
		http.Error(w, "internal target denied", http.StatusForbidden)
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
	tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{leaf.toTLS()}, MinVersion: tls.VersionTLS12})
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("proxy: tls handshake for %s: %v", host, err)
		return
	}
	defer tlsConn.Close()

	// Serve decrypted HTTP/1.1 from the client over the tunnel.
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
		resp, keepAlive := h.process(r.Context(), req, "https")
		if err := resp.Write(tlsConn); err != nil {
			return
		}
		if resp.Close || !keepAlive {
			return
		}
	}
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
func (h *Handler) Replay(ctx context.Context, rec store.Record) (*http.Response, error) {
	if rec.Capture == nil {
		return nil, fmt.Errorf("no capture")
	}
	c := rec.Capture
	req, err := http.NewRequestWithContext(ctx, c.Method, c.URL, bytes.NewReader(c.Body))
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("still blocked: %v", dec.Reasons)
	}
	if dec.Action == model.ActionSubstitute {
		applySubstitution(req, body, dec, h.Store, scanner.ScanRequest(req, scanner.Options{
			Worker: h.workerID(req), Secrets: knownSecrets(h.Store), Patterns: h.Patterns,
		}))
	}
	return h.forward(ctx, req, rec.Scheme)
}

// decide is the scan + policy step shared by process and Replay.
func (h *Handler) decide(req *http.Request, scheme string, body []byte) model.Decision {
	scan := scanner.ScanRequest(req, scanner.Options{
		Worker: h.workerID(req), Secrets: knownSecrets(h.Store), Patterns: h.Patterns,
	})
	return policy.Decide(policy.Input{
		Method: req.Method, Schema: scheme, Host: req.URL.Hostname(), Path: req.URL.Path,
		Body: string(body), Scan: scan,
		SecretByPlaceholder: placeholderMap(h.Store),
		Rules:               recRules(h.Store),
		Domains:             recDomains(h.Store),
		Exceptions:          recExceptions(h.Store),
	})
}

// process is the core: scan → decide → substitute/block → forward → record.
func (h *Handler) process(ctx context.Context, req *http.Request, scheme string) (*http.Response, bool) {
	body, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(body))

	dec := h.decide(req, scheme, body)
	scan := scanner.ScanRequest(req, scanner.Options{
		Worker: h.workerID(req), Secrets: knownSecrets(h.Store), Patterns: h.Patterns,
	})

	reqID := newID()
	rec := store.Record{
		ID:        recID(),
		Worker:    h.workerID(req),
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
		applySubstitution(req, body, dec, h.Store, scan)
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
	out.Request = req
	rec.Retryable = false
	h.Store.AppendLog(rec)
	return out, true
}

// forward replays the (possibly substituted) request upstream.
func (h *Handler) forward(ctx context.Context, req *http.Request, scheme string) (*http.Response, error) {
	target := *req.URL
	target.Scheme = scheme
	target.Host = req.URL.Host
	outReq := req.Clone(ctx)
	outReq.URL = &target
	outReq.RequestURI = ""
	return h.client.Do(outReq)
}

// applySubstitution replaces placeholder tokens with real values in headers
// and body. It re-reads the body passed from process (post-restore) — pass the
// original body bytes here since process already read them.
func applySubstitution(req *http.Request, origBody []byte, dec model.Decision, st *store.Store, scan scanner.Result) {
	pm := placeholderMap(st)
	if len(dec.Substituted) == 0 {
		return
	}
	sub := map[string]string{}
	for _, tok := range dec.Substituted {
		if s, ok := pm[tok]; ok {
			sub[tok] = s.Value
		}
	}
	if len(sub) == 0 {
		return
	}
	for _, vals := range req.Header {
		for i, v := range vals {
			nv := v
			for tok, real := range sub {
				nv = strings.ReplaceAll(nv, tok, real)
			}
			vals[i] = nv
		}
	}
	if len(origBody) > 0 {
		nb := origBody
		for tok, real := range sub {
			nb = bytes.ReplaceAll(nb, []byte(tok), []byte(real))
		}
		req.Body = io.NopCloser(bytes.NewReader(nb))
		req.ContentLength = int64(len(nb))
		req.Header.Set("Content-Length", fmt.Sprint(len(nb)))
	}
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

func placeholderMap(st *store.Store) map[string]model.Secret {
	recs := st.Secrets()
	out := make(map[string]model.Secret, len(recs))
	for _, r := range recs {
		if r.Placeholder != "" {
			out[r.Placeholder] = model.Secret{
				ID: r.ID, Name: r.Name, Placeholder: r.Placeholder, Value: r.Value,
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
			ContentRegex: e.ContentRegex, Placeholder: e.Placeholder, Enabled: e.Enabled,
		})
	}
	return out
}

func toStoreFindings(scan scanner.Result) []store.Finding {
	var out []store.Finding
	add := func(f model.Finding) {
		sf := store.Finding{Kind: string(f.Kind), PatternID: f.PatternID, SecretID: f.SecretID, Token: f.Token, Location: f.Location, Field: f.Field}
		if sf.Token != "" && len(sf.Token) > 24 {
			sf.Token = sf.Token[:12] + "…"
		}
		out = append(out, sf)
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