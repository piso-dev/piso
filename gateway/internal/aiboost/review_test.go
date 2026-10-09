package aiboost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func reviewFixture(t *testing.T) (*Client, *[]string) {
	t.Helper()
	var seen []string
	c, _, _ := fixture(t)
	connect(t, c)
	c.http.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.URL.Path)
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/api/booster-suggestions/") && r.Header.Get("Authorization") != "Bearer access-token-never-public" {
			t.Error("review reads must use the gateway bearer")
		}
		rec := httptest.NewRecorder()
		switch {
		case r.URL.Path == "/api/booster-suggestions/" && r.Method == "GET":
			io.WriteString(rec, `{"items":[{"id":"11111111-1111-1111-1111-111111111111","status":"submitted","revision":1,"name":"Go","description":"d","content":{"format":"markdown","body":"# b"}}],"next":"`+c.rec.BaseURL+`/api/booster-suggestions/?cursor=abc","previous":null}`)
		case r.URL.Path == "/api/booster-suggestions/11111111-1111-1111-1111-111111111111/" && r.Method == "GET":
			io.WriteString(rec, `{"id":"11111111-1111-1111-1111-111111111111","status":"submitted","revision":2,"name":"Go","description":"d","content":{"format":"markdown","body":"# b"}}`)
		case r.URL.Path == "/api/booster-suggestions/11111111-1111-1111-1111-111111111111/approve/" && r.Method == "POST":
			if r.Header.Get("Authorization") != "Bearer access-token-never-public" {
				t.Error("decision must use the same gateway bearer")
			}
			if r.URL.Query().Encode() != "" {
				t.Error("decision calls must not forward query parameters")
			}
			var body Decision
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.ExpectedRevision != 2 {
				t.Error("decision must carry the displayed revision")
			}
			io.WriteString(rec, `{"id":"11111111-1111-1111-1111-111111111111","status":"converted","revision":2,"name":"Go","description":"d","content":{"format":"markdown","body":"# b"}}`)
		default:
			rec.WriteHeader(404)
		}
		rec.Header().Set("Content-Type", "application/json")
		return rec.Result(), nil
	})
	return c, &seen
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSuggestionsPageMapping(t *testing.T) {
	c, _ := reviewFixture(t)
	page, err := c.Suggestions(context.Background(), "submitted", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor != "abc" || page.PreviousCursor != "" {
		t.Fatal("cursor mapping failed", page)
	}
	if _, err = c.Suggestions(context.Background(), "bogus", ""); err == nil {
		t.Fatal("unknown status accepted")
	}
}
func TestGetSuggestion(t *testing.T) {
	c, _ := reviewFixture(t)
	item, err := c.GetSuggestion(context.Background(), "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if item.Revision != 2 || item.Content.Body != "# b" {
		t.Fatal("suggestion payload mismatch", item)
	}
	if _, err = c.GetSuggestion(context.Background(), "nope"); err == nil {
		t.Fatal("non-UUID accepted")
	}
}
func TestDecideUsesGatewayCredential(t *testing.T) {
	c, seen := reviewFixture(t)
	out, err := c.Decide(context.Background(), "11111111-1111-1111-1111-111111111111", "approve", Decision{ExpectedRevision: 2, Feedback: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "converted" {
		t.Fatal("decision result mismatch", out)
	}
	if len(*seen) != 1 || (*seen)[0] != "/api/booster-suggestions/11111111-1111-1111-1111-111111111111/approve/" {
		t.Fatal("expected one decision request", *seen)
	}
}
func TestDecideValidatesInput(t *testing.T) {
	c, _ := reviewFixture(t)
	if _, err := c.Decide(context.Background(), "nope", "approve", Decision{ExpectedRevision: 1}); err == nil {
		t.Fatal("bad ID accepted")
	}
	if _, err := c.Decide(context.Background(), "11111111-1111-1111-1111-111111111111", "publish", Decision{ExpectedRevision: 1}); err == nil {
		t.Fatal("unknown action accepted")
	}
	if _, err := c.Decide(context.Background(), "11111111-1111-1111-1111-111111111111", "approve", Decision{ExpectedRevision: 0}); err == nil {
		t.Fatal("missing expectedRevision accepted")
	}
}
