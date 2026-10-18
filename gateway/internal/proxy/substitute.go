package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"

	"piso/gateway/internal/scanner"
)

// errUnsupportedSite means a placeholder needed substitution in a body the
// gateway cannot rewrite field-safely (opaque binary, malformed JSON, a form
// parameter whose name is payload). The request is blocked instead of falling
// back to a blind find-and-replace: that fallback is exactly how a real
// credential leaks into model input, an issue body, or a gist.
var errUnsupportedSite = errors.New("unsupported substitution site")

// substituteJSONSkippingChat rewrites string values in a JSON body only at
// credential sites (see scanner.JSONFieldIsCredentialSite). Payload fields —
// messages[], system, instructions, input, contents, tools[].description and
// any unrecognized field — are left byte-for-byte as text. Non-JSON input
// returns ok=false.
func substituteJSONSkippingChat(body []byte, sub map[string]string) ([]byte, bool) {
	t := bytes.TrimSpace(body)
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') {
		return nil, false
	}
	return rewriteJSONCredentialSites(body, sub)
}

// rewriteJSONCredentialSites rewrites JSON with a token stream instead of
// decoding into map[string]any. That preserves object key order, duplicate
// keys, and json.Number literals (a decode/re-encode pass silently reorders
// and normalizes them, which can break provider requests). Whitespace is
// compacted; string contents keep their meaning.
func rewriteJSONCredentialSites(body []byte, sub map[string]string) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var stack []jsonFrame
	var out bytes.Buffer
	changed := false

	childPath := func() string {
		f := topFrame(stack)
		if f == nil {
			return ""
		}
		if f.kind == '{' {
			if f.path == "" {
				return f.key
			}
			return f.path + "." + f.key
		}
		if f.path == "" {
			return "[" + strconv.Itoa(f.n) + "]"
		}
		return f.path + "[" + strconv.Itoa(f.n) + "]"
	}
	// beforeValue writes the separator a JSON array needs and reserves the
	// element slot. Object separators live between key and colon (written when
	// the key is emitted), so objects need nothing here.
	beforeValue := func() {
		f := topFrame(stack)
		if f == nil {
			return
		}
		if f.kind == '[' {
			if f.n > 0 {
				out.WriteByte(',')
			}
			f.n++
		}
	}
	// afterValue returns an object to "expecting a key" after its value.
	afterValue := func() {
		if f := topFrame(stack); f != nil && f.kind == '{' {
			f.expectKey = true
		}
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{', '[':
				p := childPath()
				beforeValue()
				afterValue()
				stack = append(stack, jsonFrame{kind: byte(v), path: p, expectKey: v == '{'})
				out.WriteByte(byte(v))
			case '}', ']':
				if len(stack) == 0 {
					return nil, false
				}
				stack = stack[:len(stack)-1]
				out.WriteByte(byte(v))
			default:
				return nil, false
			}
		case string:
			if f := topFrame(stack); f != nil && f.kind == '{' && f.expectKey {
				if f.n > 0 {
					out.WriteByte(',')
				}
				f.n++
				f.key = v
				f.expectKey = false
				writeJSONString(&out, v)
				out.WriteByte(':')
				continue
			}
			p := childPath()
			if scanner.JSONFieldIsCredentialSite(p) {
				for tok, real := range sub {
					if nv := strings.ReplaceAll(v, tok, real); nv != v {
						v = nv
						changed = true
					}
				}
			}
			beforeValue()
			writeJSONString(&out, v)
			afterValue()
		case json.Number:
			beforeValue()
			out.WriteString(v.String())
			afterValue()
		case bool:
			beforeValue()
			if v {
				out.WriteString("true")
			} else {
				out.WriteString("false")
			}
			afterValue()
		case nil:
			beforeValue()
			out.WriteString("null")
			afterValue()
		default:
			return nil, false
		}
	}
	if len(stack) != 0 {
		return nil, false
	}
	if !changed {
		// No credential site carried a placeholder. Return the original bytes
		// instead of a compacted re-serialization: prompt caching and signed
		// payloads care about byte identity.
		return body, true
	}
	return out.Bytes(), true
}

// jsonFrame tracks one open JSON container while rewriting.
type jsonFrame struct {
	kind      byte // '{' or '['
	path      string
	key       string
	n         int // entries so far (keys for objects, elements for arrays)
	expectKey bool
}

func topFrame(stack []jsonFrame) *jsonFrame {
	if len(stack) == 0 {
		return nil
	}
	return &stack[len(stack)-1]
}

// writeJSONString appends s as a JSON string literal without HTML escaping.
func writeJSONString(dst *bytes.Buffer, s string) {
	enc := json.NewEncoder(dst)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	if b := dst.Bytes(); len(b) > 0 && b[len(b)-1] == '\n' {
		dst.Truncate(len(b) - 1)
	}
}

// substituteForm rewrites only credential-named form parameters. Every other
// parameter is payload and is forwarded untouched. Like the query rewriter it
// edits segments in place, so parameter order and encoding survive (a
// url.Values round-trip reorders and re-escapes, which breaks signed forms).
func substituteForm(body []byte, sub map[string]string) []byte {
	return []byte(rewriteEncodedPairs(string(body), url.QueryEscape, sub))
}

// rewriteEncodedPairs rewrites placeholder occurrences inside the values of
// credential-named `k=v&k=v` pairs. The name is percent-decoded for the site
// check; the value bytes are preserved apart from the substitution.
func rewriteEncodedPairs(raw string, escape func(string) string, sub map[string]string) string {
	segs := strings.Split(raw, "&")
	changed := false
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		k, v, hasEq := strings.Cut(seg, "=")
		if !hasEq {
			continue
		}
		uk, err := url.QueryUnescape(k)
		if err != nil {
			uk = k
		}
		if !scanner.QueryParamIsCredentialSite(uk) {
			continue
		}
		nv := v
		for tok, real := range sub {
			nv = strings.ReplaceAll(nv, tok, escape(real))
		}
		if nv != v {
			segs[i] = k + "=" + nv
			changed = true
		}
	}
	if !changed {
		return raw
	}
	return strings.Join(segs, "&")
}

// containsAnyToken reports whether body contains any placeholder being
// substituted. Used to skip rewriting bodies that do not need it.
func containsAnyToken(body []byte, sub map[string]string) bool {
	for tok := range sub {
		if bytes.Contains(body, []byte(tok)) {
			return true
		}
	}
	return false
}
