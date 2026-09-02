// Package scanner inspects decoded HTTP requests (headers, query, body) for
// piso_ placeholders, exact-known real credentials, and credential-looking
// patterns. It is the front end of the block/substitute decision.
package scanner

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"piso/gateway/internal/model"
	"piso/gateway/internal/patterns"
)

// PlaceholderRegex matches piso_ tokens. Placeholders are the ONLY credential
// form the worker is allowed to emit.
var PlaceholderRegex = regexp.MustCompile(`\bpiso_[A-Za-z0-9_.-]{6,}\b`)

// Options configures a scan.
type Options struct {
	// Worker identifies the request source for the log.
	Worker string
	// Secrets are exact-known real credentials (value + id). Matching any of
	// these values anywhere in the request is a hard block.
	Secrets []KnownSecret
	// Patterns is the credential-looking regex library.
	Patterns *patterns.Compiled
}

// KnownSecret is the exact-match input (id + value; never logged raw).
type KnownSecret struct {
	ID    string
	Value string
}

// Result is everything the scanner found.
type Result struct {
	Placeholders []model.Finding // piso_ tokens found
	RealSecrets  []model.Finding // exact known real credential found
	PatternHits  []model.Finding // regex hits
}

// HasPlaceholder reports whether any piso_ token was found.
func (r Result) HasPlaceholder() bool { return len(r.Placeholders) > 0 }

// LooksCredential reports whether real credentials or patterns were seen.
func (r Result) LooksCredential() bool { return len(r.RealSecrets) > 0 || len(r.PatternHits) > 0 }

// ScanRequest scans a decoded HTTP request.
func ScanRequest(req *http.Request, opts Options) Result {
	var res Result

	// Headers (hop-by-hop noise was already stripped by the MITM layer).
	for name, vals := range req.Header {
		loc := locationForHeader(name)
		for _, v := range vals {
			scanTextInto(&res, v, opts, loc, name)
		}
	}

	// Query string.
	if q := req.URL.RawQuery; q != "" {
		scanTextInto(&res, q, opts, "query", "")
	}

	// Body: read fully (the MITM layer needs the bytes anyway), then restore.
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	if len(body) > 0 {
		ct := req.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/json") {
			if scanJSON(body, opts, &res) {
				return res
			}
		}
		scanTextInto(&res, string(body), opts, "body", "")
	}
	return res
}

// scanTextInto runs all three detectors on one text chunk.
func scanTextInto(res *Result, text string, opts Options, loc, field string) {
	for _, tok := range PlaceholderRegex.FindAllString(text, -1) {
		res.Placeholders = append(res.Placeholders, model.Finding{
			Kind: model.FindingPlaceholder, Token: tok, Location: loc, Field: field,
		})
	}
	if len(opts.Secrets) > 0 {
		low := strings.ToLower(text)
		for _, s := range opts.Secrets {
			if strings.Contains(low, strings.ToLower(s.Value)) {
				res.RealSecrets = append(res.RealSecrets, model.Finding{
					Kind: model.FindingRealSecret, SecretID: s.ID, Token: redact(s.Value), Location: loc, Field: field,
				})
			}
		}
	}
	if opts.Patterns != nil {
		for _, m := range opts.Patterns.Match([]byte(text)) {
			res.PatternHits = append(res.PatternHits, model.Finding{
				Kind: model.FindingPatternSecret, PatternID: m.PatternID, Token: m.Sample, Location: loc, Field: field,
			})
		}
	}
}

// scanJSON scans JSON bodies with field paths; returns true if the body was
// valid JSON (already scanned) and the caller should not re-scan raw.
func scanJSON(body []byte, opts Options, res *Result) bool {
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return false
	}
	walkJSON("", raw, func(path, val string) {
		scanTextInto(res, val, opts, "json-body", path)
	})
	return true
}

func walkJSON(path string, v any, visit func(path, val string)) {
	switch t := v.(type) {
	case map[string]any:
		for k, cv := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			walkJSON(p, cv, visit)
		}
	case []any:
		for i, cv := range t {
			walkJSON(path+"["+strconv.Itoa(i)+"]", cv, visit)
		}
	case string:
		visit(path, t)
	}
}

// redact returns a short prefix/suffix of a real secret for log purposes —
// never the full value.
func redact(v string) string {
	if len(v) <= 8 {
		return "***"
	}
	return v[:4] + "…" + v[len(v)-4:]
}

// locationForHeader normalizes header names into stable location labels.
func locationForHeader(name string) string {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization":
		return "authorization"
	case "X-Api-Key":
		return "x-api-key"
	case "Cookie":
		return "cookie"
	default:
		return "header:" + strings.ToLower(name)
	}
}