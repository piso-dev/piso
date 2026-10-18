package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Response redaction is a backstop, not the primary control. The request-side
// site allowlist decides where a credential may be written; this layer catches
// a real secret coming BACK from an origin: a model echoing a substituted key,
// an error page, a GitHub issue body, an API response that reflects auth.
//
// It is exact-match on values already known to the vault (plus GitHub App PEMs
// and minted installation tokens). A model that splits or encodes a secret can
// still evade it — which is why the substitution-site rules remain the fix.

type redactTarget struct {
	value       []byte
	prefix      []byte // value truncated to holdWindow, for boundary detection
	replacement []byte
	secretID    string
}

// holdWindow bounds how many trailing bytes the streaming redactor may hold
// back while waiting for a possible secret straddling a chunk boundary. It is
// a latency/CPU bound only: a secret longer than this cannot be held whole,
// but only its first holdWindow bytes could reach the client in that case —
// for a long PEM that is a header line, not key material.
const holdWindow = 256

type redactor struct {
	targets []redactTarget // longest value first
	onLeak  func(location, secretID string)

	mu   sync.Mutex
	seen map[string]bool
}

// newRedactor builds a redactor from every real secret the gateway knows.
// Returns nil when there is nothing to redact.
func (h *Handler) newRedactor() *redactor {
	pm := placeholderMap(h.Store)
	phByID := make(map[string]string, len(pm))
	for ph, s := range pm {
		if s.ID != "" {
			phByID[s.ID] = ph
		}
	}
	seenVal := map[string]bool{}
	var targets []redactTarget
	for _, ks := range h.scanSecrets() {
		if len(ks.Value) < 8 {
			// Too short to match without drowning normal text in false hits.
			continue
		}
		if seenVal[ks.Value] {
			continue
		}
		seenVal[ks.Value] = true
		repl := phByID[ks.ID]
		if repl == "" {
			repl = "[piso:redacted]"
		}
		val := []byte(ks.Value)
		pfx := val
		if len(pfx) > holdWindow {
			pfx = pfx[:holdWindow]
		}
		targets = append(targets, redactTarget{
			value:       val,
			prefix:      pfx,
			replacement: []byte(repl),
			secretID:    ks.ID,
		})
	}
	if len(targets) == 0 {
		return nil
	}
	// Longest first: a secret that contains another secret must win.
	sort.Slice(targets, func(i, j int) bool {
		return len(targets[i].value) > len(targets[j].value)
	})
	return &redactor{targets: targets, seen: map[string]bool{}}
}

func (r *redactor) record(location, secretID string) {
	if r.onLeak == nil {
		return
	}
	key := location + "\x00" + secretID
	r.mu.Lock()
	if r.seen[key] {
		r.mu.Unlock()
		return
	}
	r.seen[key] = true
	r.mu.Unlock()
	r.onLeak(location, secretID)
}

// redact replaces every complete target occurrence in data. A partial match at
// the end is the caller's responsibility (see holdLen).
func (r *redactor) redact(data []byte, location string) []byte {
	if len(data) == 0 {
		return data
	}
	found := false
	for _, t := range r.targets {
		if bytes.Contains(data, t.value) {
			found = true
			break
		}
	}
	if !found {
		return data
	}
	var out bytes.Buffer
	out.Grow(len(data))
	for i := 0; i < len(data); {
		matched := false
		for ti := range r.targets {
			t := &r.targets[ti]
			if bytes.HasPrefix(data[i:], t.value) {
				out.Write(t.replacement)
				r.record(location, t.secretID)
				i += len(t.value)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		out.WriteByte(data[i])
		i++
	}
	return out.Bytes()
}

// holdLen is the longest suffix of buf that is a proper prefix of some target.
// Holding only that suffix (rather than a fixed window) keeps SSE token
// streaming snappy: normal text matches no prefix and is emitted at once.
func (r *redactor) holdLen(buf []byte) int {
	hold := 0
	for _, t := range r.targets {
		max := len(t.prefix) - 1
		if max > len(buf) {
			max = len(buf)
		}
		for k := max; k > hold; k-- {
			if bytes.HasSuffix(buf, t.prefix[:k]) {
				hold = k
				break
			}
		}
	}
	return hold
}

// redactedHeaders rewrites secret values reflected in response headers
// (Location, Set-Cookie, custom echo headers).
func redactedHeaders(hdr http.Header, r *redactor) {
	for name, vals := range hdr {
		loc := "response:header:" + strings.ToLower(name)
		for i, v := range vals {
			if out := r.redact([]byte(v), loc); !bytes.Equal(out, []byte(v)) {
				vals[i] = string(out)
			}
		}
	}
}

// redactReader streams a response body through the redactor. Bytes that could
// be the start of a known secret are held until resolved or EOF.
type redactReader struct {
	src io.ReadCloser
	r   *redactor
	loc string

	buf []byte
	out []byte
	tmp []byte
	eof bool
}

func newRedactReader(src io.ReadCloser, r *redactor, loc string) *redactReader {
	return &redactReader{src: src, r: r, loc: loc}
}

func (rr *redactReader) Read(p []byte) (int, error) {
	for {
		if len(rr.out) > 0 {
			n := copy(p, rr.out)
			rr.out = rr.out[n:]
			return n, nil
		}
		if rr.eof {
			return 0, io.EOF
		}
		if rr.tmp == nil {
			rr.tmp = make([]byte, 32*1024)
		}
		n, err := rr.src.Read(rr.tmp)
		if n > 0 {
			rr.buf = append(rr.buf, rr.tmp[:n]...)
		}
		if err != nil && err != io.EOF {
			return 0, err
		}
		rr.eof = err == io.EOF
		if len(rr.buf) > 0 {
			if rr.eof {
				rr.out = rr.r.redact(rr.buf, rr.loc)
				rr.buf = nil
			} else {
				hold := rr.r.holdLen(rr.buf)
				if len(rr.buf) > hold {
					cut := len(rr.buf) - hold
					rr.out = rr.r.redact(rr.buf[:cut], rr.loc)
					// Copy the tail so a large read does not pin its backing array.
					rr.buf = append([]byte(nil), rr.buf[cut:]...)
				}
			}
		}
		if len(rr.out) == 0 && rr.eof {
			return 0, io.EOF
		}
		// len(out)==0 and not EOF: read more from the source.
	}
}

func (rr *redactReader) Close() error { return rr.src.Close() }

// protectResponse wraps an origin response so known real secrets are replaced
// with their placeholders in both headers and body. onLeak is optional (the
// replay path has no per-request log row of its own).
func (h *Handler) protectResponse(out *http.Response, onLeak func(location, secretID string)) {
	if out == nil {
		return
	}
	red := h.newRedactor()
	if red == nil {
		return
	}
	red.onLeak = onLeak
	redactedHeaders(out.Header, red)
	if !responseHasBody(out) || out.Body == nil || !bodyNeedsRedaction(out) {
		return
	}
	plain, wasGzip := gunzipBody(out.Body)
	out.Body = newRedactReader(plain, red, "response:body")
	if wasGzip {
		out.Header.Del("Content-Encoding")
	}
	out.Header.Del("Content-Length")
	out.ContentLength = -1
}

// bodyNeedsRedaction skips bodies that cannot plausibly carry a reflected
// credential as text (tarballs, images, fonts). The cost is one exact-match
// scan per known secret across the whole body, which is not free for large
// downloads; headers are still scanned. Unknown/missing types are scanned.
func bodyNeedsRedaction(out *http.Response) bool {
	if out.Header.Get("Content-Encoding") != "" {
		return true
	}
	ct := strings.ToLower(strings.TrimSpace(out.Header.Get("Content-Type")))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if ct == "" {
		return true
	}
	for _, marker := range []string{
		"text/", "json", "xml", "javascript", "ecmascript",
		"x-www-form-urlencoded", "event-stream", "graphql",
	} {
		if strings.Contains(ct, marker) {
			return true
		}
	}
	return false
}

// gunzipBody transparently decompresses a gzip response body so the redactor
// can see plain text. Origins are asked for identity encoding, but some ignore
// that. Detection is by magic bytes, so a mislabeled body is never re-encoded
// wrongly; non-gzip bodies come back wrapped with their buffered bytes intact.
func gunzipBody(body io.ReadCloser) (io.ReadCloser, bool) {
	br := bufio.NewReader(body)
	magic, err := br.Peek(2)
	if err != nil || len(magic) < 2 || magic[0] != 0x1f || magic[1] != 0x8b {
		return struct {
			io.Reader
			io.Closer
		}{br, body}, false
	}
	gz, err := gzip.NewReader(br)
	if err != nil {
		return struct {
			io.Reader
			io.Closer
		}{br, body}, false
	}
	return &gzipReadCloser{Reader: gz, gz: gz, src: body}, true
}

type gzipReadCloser struct {
	io.Reader
	gz  *gzip.Reader
	src io.ReadCloser
}

func (g *gzipReadCloser) Close() error {
	_ = g.gz.Close()
	return g.src.Close()
}
