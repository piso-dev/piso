package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"piso/gateway/internal/model"
	"piso/gateway/internal/scanner"
	"piso/gateway/internal/store"
)

func newSiteStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

const (
	sitePlaceholder = "piso_gh_abc123def456"
	siteReal        = "github_pat_REALVALUE0123456789"
)

func siteStoreWithSecret(t *testing.T) *store.Store {
	t.Helper()
	st := newSiteStore(t)
	if err := st.AddSecret(store.SecretRec{
		ID: "s1", Name: "github", Placeholder: sitePlaceholder, Value: siteReal,
		AllowedHosts: []string{"api.example.test"},
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func applyAndRead(t *testing.T, req *http.Request, body []byte) ([]byte, error) {
	t.Helper()
	st := siteStoreWithSecret(t)
	req.Body = io.NopCloser(bytes.NewReader(body))
	err := applySubstitution(req, body, model.Decision{Substituted: []string{sitePlaceholder}}, st, scanner.Result{}, nil)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || req.Body == nil {
		return nil, nil
	}
	return io.ReadAll(req.Body)
}

// Every provider puts model-visible text somewhere different. Substitution
// must rewrite only the credential leaf and leave all of it byte-identical.
func TestApplySubstitutionJSONOnlyCredentialSites(t *testing.T) {
	body := []byte(`{"model":"x","system":"sys ` + sitePlaceholder + `","instructions":"ins ` + sitePlaceholder + `",` +
		`"input":[{"content":"in ` + sitePlaceholder + `"}],` +
		`"messages":[{"role":"user","content":"use ` + sitePlaceholder + `"}],` +
		`"tools":[{"type":"function","function":{"description":"desc ` + sitePlaceholder + `"}}],` +
		`"api_key":"` + sitePlaceholder + `"}`)
	req, _ := http.NewRequest("POST", "https://api.example.test/v1/responses", nil)
	out, err := applyAndRead(t, req, body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(siteReal)) && bytes.Count(out, []byte(siteReal)) != 1 {
		t.Fatalf("real secret written to a payload field: %s", out)
	}
	if !bytes.Contains(out, []byte(`"api_key":"`+siteReal+`"`)) {
		t.Fatalf("api_key was not substituted: %s", out)
	}
	for _, payload := range []string{
		`"system":"sys ` + sitePlaceholder + `"`,
		`"instructions":"ins ` + sitePlaceholder + `"`,
		`"content":"in ` + sitePlaceholder + `"`,
		`"content":"use ` + sitePlaceholder + `"`,
		`"description":"desc ` + sitePlaceholder + `"`,
	} {
		if !bytes.Contains(out, []byte(payload)) {
			t.Errorf("payload field rewritten, want %s in %s", payload, out)
		}
	}
	if !json.Valid(out) {
		t.Fatalf("rewrite is not valid JSON: %s", out)
	}
}

// A decode/re-encode pass would reorder keys, drop duplicate keys and rewrite
// numbers. The token-stream rewriter must not.
func TestApplySubstitutionJSONPreservesShape(t *testing.T) {
	body := []byte(`{"z":1,"a":{"api_key":"` + sitePlaceholder + `"},"z":2,"n":1.50}`)
	req, _ := http.NewRequest("POST", "https://api.example.test/v1", nil)
	out, err := applyAndRead(t, req, body)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"z":1,"a":{"api_key":"` + siteReal + `"},"z":2,"n":1.50}`
	if string(out) != want {
		t.Fatalf("rewrite changed shape:\n got %s\nwant %s", out, want)
	}
}

// Opaque bodies used to fall back to bytes.ReplaceAll — the exact leak path
// for an issue body or a gist. They must block instead.
func TestApplySubstitutionOpaqueBodyBlocks(t *testing.T) {
	body := []byte("model prompt mentioning " + sitePlaceholder + " verbatim")
	req, _ := http.NewRequest("POST", "https://api.example.test/v1", nil)
	req.Header.Set("Content-Type", "text/plain")
	_, err := applyAndRead(t, req, body)
	if !errors.Is(err, errUnsupportedSite) {
		t.Fatalf("want errUnsupportedSite, got %v", err)
	}
}

func TestApplySubstitutionMalformedJSONBlocks(t *testing.T) {
	body := []byte(`{"api_key":"` + sitePlaceholder + `"`)
	req, _ := http.NewRequest("POST", "https://api.example.test/v1", nil)
	_, err := applyAndRead(t, req, body)
	if !errors.Is(err, errUnsupportedSite) {
		t.Fatalf("want errUnsupportedSite, got %v", err)
	}
}

func TestApplySubstitutionFormOnlyCredentialKeys(t *testing.T) {
	body := []byte("api_key=" + sitePlaceholder + "&comment=" + sitePlaceholder)
	req, _ := http.NewRequest("POST", "https://api.example.test/v1", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	out, err := applyAndRead(t, req, body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("api_key="+siteReal)) {
		t.Fatalf("api_key not substituted: %s", out)
	}
	if !bytes.Contains(out, []byte("comment="+sitePlaceholder)) {
		t.Fatalf("form comment rewritten: %s", out)
	}
}

func TestApplySubstitutionLeavesPayloadHeadersAndQuery(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.example.test/v1?token="+sitePlaceholder+"&q="+sitePlaceholder, nil)
	req.Header.Set("User-Agent", "agent/1 "+sitePlaceholder)
	req.Header.Set("X-Title", sitePlaceholder)
	req.Header.Set("X-Api-Key", sitePlaceholder)
	req.Header.Set("Authorization", "Bearer "+sitePlaceholder)
	out, err := applyAndRead(t, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("empty body should stay nil, got %q", out)
	}
	if got := req.Header.Get("User-Agent"); !strings.Contains(got, sitePlaceholder) {
		t.Fatalf("User-Agent was rewritten: %q", got)
	}
	if got := req.Header.Get("X-Title"); got != sitePlaceholder {
		t.Fatalf("X-Title was rewritten: %q", got)
	}
	if got := req.Header.Get("X-Api-Key"); got != siteReal {
		t.Fatalf("X-Api-Key not substituted: %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+siteReal {
		t.Fatalf("Authorization not substituted: %q", got)
	}
	if got := req.URL.Query().Get("token"); got != siteReal {
		t.Fatalf("token query not substituted: %q", got)
	}
	if got := req.URL.Query().Get("q"); got != sitePlaceholder {
		t.Fatalf("payload query rewritten: %q", got)
	}
}

// ---- response redaction ----

type chunkReader struct {
	chunks [][]byte
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[0])
	if n == len(c.chunks[0]) {
		c.chunks = c.chunks[1:]
	} else {
		c.chunks[0] = c.chunks[0][n:]
	}
	return n, nil
}

func TestRedactorRewritesSecretSplitAcrossChunks(t *testing.T) {
	h := &Handler{Store: siteStoreWithSecret(t)}
	red := h.newRedactor()
	if red == nil {
		t.Fatal("expected a redactor")
	}
	// One contiguous secret in an SSE frame; vary the network chunk size so a
	// chunk boundary lands inside the secret for every possible offset.
	stream := "data: {\"delta\":\"prefix " + siteReal + " suffix\"}\n\n"
	for size := 1; size <= len(siteReal)+1; size++ {
		var chunks [][]byte
		for i := 0; i < len(stream); i += size {
			end := i + size
			if end > len(stream) {
				end = len(stream)
			}
			chunks = append(chunks, []byte(stream[i:end]))
		}
		rr := newRedactReader(io.NopCloser(&chunkReader{chunks: chunks}), red, "response:body")
		got, err := io.ReadAll(rr)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(got, []byte(siteReal)) {
			t.Fatalf("chunk size %d: secret survived redaction: %s", size, got)
		}
		if !bytes.Contains(got, []byte(sitePlaceholder)) {
			t.Fatalf("chunk size %d: placeholder missing: %s", size, got)
		}
		if !bytes.Contains(got, []byte("prefix ")) || !bytes.Contains(got, []byte(" suffix")) {
			t.Fatalf("chunk size %d: surrounding text lost: %s", size, got)
		}
	}
}

func TestRedactorLeavesCleanStreamByteIdentical(t *testing.T) {
	h := &Handler{Store: siteStoreWithSecret(t)}
	red := h.newRedactor()
	stream := "data: {\"delta\":\"hello world\"}\n\n"
	rr := newRedactReader(io.NopCloser(strings.NewReader(stream)), red, "response:body")
	got, err := io.ReadAll(rr)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != stream {
		t.Fatalf("clean stream changed:\n got %q\nwant %q", got, stream)
	}
}

func TestRedactedHeadersCoversLocationAndSetCookie(t *testing.T) {
	h := &Handler{Store: siteStoreWithSecret(t)}
	red := h.newRedactor()
	hdr := http.Header{
		"Location":   {"https://evil.test/?k=" + siteReal},
		"Set-Cookie": {"sid=" + siteReal + "; Path=/"},
	}
	redactedHeaders(hdr, red)
	for name, vals := range hdr {
		for _, v := range vals {
			if strings.Contains(v, siteReal) {
				t.Fatalf("%s still carries the real secret: %q", name, v)
			}
			if !strings.Contains(v, sitePlaceholder) {
				t.Fatalf("%s missing placeholder: %q", name, v)
			}
		}
	}
}

func TestRedactorSkipsTooShortValues(t *testing.T) {
	st := newSiteStore(t)
	if err := st.AddSecret(store.SecretRec{ID: "s1", Name: "pin", Placeholder: "piso_pin_abcdef", Value: "1234"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Store: st}
	if red := h.newRedactor(); red != nil {
		t.Fatal("short value must not be redacted (false positives on normal text)")
	}
}

func TestProtectResponseRedactsBodyHeadersAndLogsLeak(t *testing.T) {
	h := &Handler{Store: siteStoreWithSecret(t)}
	out := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"Location":     {"https://x.test/?k=" + siteReal},
		},
		Body: io.NopCloser(strings.NewReader(`{"echo":"` + siteReal + `","again":"` + siteReal + `"}`)),
	}
	var leaks []string
	h.protectResponse(out, func(location, secretID string) {
		leaks = append(leaks, location+"/"+secretID)
	})
	body, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(siteReal)) {
		t.Fatalf("body still carries the secret: %s", body)
	}
	if bytes.Count(body, []byte(sitePlaceholder)) != 2 {
		t.Fatalf("want both occurrences replaced: %s", body)
	}
	if strings.Contains(out.Header.Get("Location"), siteReal) {
		t.Fatalf("Location still carries the secret: %q", out.Header.Get("Location"))
	}
	// One log row per location, not per occurrence.
	if len(leaks) != 2 {
		t.Fatalf("want leak per location (body, location), got %v", leaks)
	}
}

func TestProtectResponseDecompressesGzip(t *testing.T) {
	h := &Handler{Store: siteStoreWithSecret(t)}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte("data: " + siteReal + "\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	out := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Encoding": {"gzip"}, "Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(gz.Bytes())),
	}
	h.protectResponse(out, nil)
	body, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), siteReal) {
		t.Fatalf("gzip body still carries the secret: %s", body)
	}
	if !strings.Contains(string(body), sitePlaceholder) {
		t.Fatalf("gzip body not redacted: %s", body)
	}
	if out.Header.Get("Content-Encoding") != "" {
		t.Fatal("Content-Encoding must be dropped after decompression")
	}
}

// A prompt-only occurrence must leave the body byte-identical (not even
// re-serialized): model prompt caching and signatures depend on exact bytes.
func TestApplySubstitutionPromptOnlyBodyUntouched(t *testing.T) {
	body := []byte("{ \"messages\" : [ {\"role\":\"user\", \"content\":\"keep " + sitePlaceholder + "\"} ] }")
	req, _ := http.NewRequest("POST", "https://api.example.test/v1/chat", nil)
	req.Header.Set("Authorization", "Bearer "+sitePlaceholder)
	out, err := applyAndRead(t, req, body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("prompt-only body changed:\n got %s\nwant %s", out, body)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+siteReal {
		t.Fatalf("header not substituted: %q", got)
	}
}
