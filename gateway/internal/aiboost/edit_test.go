package aiboost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const editID = "33333333-3333-3333-3333-333333333333"

func TestEditDraftPartialPayload(t *testing.T) {
	for _, payload := range []string{
		`{"expectedRevision":2,"name":"New name"}`,
		`{"expectedRevision":2,"baseVersionId":"11111111-1111-1111-1111-111111111111","version":"1.2.0","changelog":"Rebased"}`,
		`{"expectedRevision":2,"version":"1.2.0"}`,
		`{"expectedRevision":2,"changelog":"New checks"}`,
		`{"expectedRevision":2,"suggestedTags":[]}`,
		`{"expectedRevision":2,"content":{"format":"markdown","body":"\n# New\n  preserved  \n"}}`,
		`{"expectedRevision":2,"changeSummary":"why","description":"New description","rationale":"New rationale","suggestedTags":["tag"]}`,
	} {
		t.Run(payload, func(t *testing.T) {
			c, _, _ := fixture(t)
			connect(t, c)
			calls := 0
			c.http.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "PATCH" || r.URL.Path != "/api/drafts/"+editID+"/" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer access-token-never-public" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("Content-Type") != "application/json" {
					t.Fatal("incorrect edit request")
				}
				body, _ := io.ReadAll(r.Body)
				var want, got map[string]any
				json.Unmarshal([]byte(payload), &want)
				json.Unmarshal(body, &got)
				w, _ := json.Marshal(want)
				g, _ := json.Marshal(got)
				if string(w) != string(g) {
					t.Fatalf("got %s, want %s", g, w)
				}
				rec := httptest.NewRecorder()
				fmt.Fprintf(rec, `{"id":%q,"status":"submitted","revision":3}`, editID)
				return rec.Result(), nil
			})
			var edit DraftEdit
			if err := json.Unmarshal([]byte(payload), &edit); err != nil {
				t.Fatal(err)
			}
			out, err := c.EditDraft(context.Background(), editID, edit)
			if err != nil || out.ID != editID || out.Revision != 3 || calls != 1 {
				t.Fatal(out, err, calls)
			}
		})
	}
}

func TestEditDraftValidationBeforeNetwork(t *testing.T) {
	c, _, _ := fixture(t)
	c.http.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid edit reached network")
		return nil, nil
	})
	str := func(s string) *string { return &s }
	for _, edit := range []DraftEdit{
		{ExpectedRevision: 0, Name: str("name")},
		{ExpectedRevision: 1},
		{ExpectedRevision: 1, ChangeSummary: "summary alone"},
		{ExpectedRevision: 1, Name: str("")},
		{ExpectedRevision: 1, Name: str(strings.Repeat("界", 256))},
		{ExpectedRevision: 1, Description: str(" ")},
		{ExpectedRevision: 1, Rationale: str(strings.Repeat("x", 10001))},
		{ExpectedRevision: 1, Content: &Content{Format: "html", Body: "hi"}},
		{ExpectedRevision: 1, Content: &Content{Format: "markdown", Body: strings.Repeat("界", 33334)}},
		{ExpectedRevision: 1, Content: &Content{Format: "markdown", Body: " \n"}},
		{ExpectedRevision: 1, SuggestedTags: &[]string{""}},
		{ExpectedRevision: 1, SuggestedTags: &[]string{strings.Repeat("x", 101)}},
		{ExpectedRevision: 1, SuggestedTags: new([]string)},
		{ExpectedRevision: 1, Name: str("name"), ChangeSummary: strings.Repeat("x", 2001)},
	} {
		if _, err := c.EditDraft(context.Background(), editID, edit); err == nil {
			t.Fatal("accepted invalid edit", edit)
		}
	}
	if _, err := c.EditDraft(context.Background(), "../approve", DraftEdit{ExpectedRevision: 1, Name: str("name")}); err == nil {
		t.Fatal("accepted invalid ID")
	}
}

func TestEditDraftNoReplayOrLeak(t *testing.T) {
	for _, status := range []int{301, 307, 308, 400, 401, 403, 404, 409, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c, _, _ := fixture(t)
			connect(t, c)
			calls := 0
			c.http.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				rec := httptest.NewRecorder()
				rec.Header().Set("Location", "https://untrusted.example/approve/")
				rec.WriteHeader(status)
				io.WriteString(rec, "sensitive upstream body")
				return rec.Result(), nil
			})
			name := "updated"
			_, err := c.EditDraft(context.Background(), editID, DraftEdit{ExpectedRevision: 1, Name: &name})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != status || calls != 1 || strings.Contains(err.Error(), "sensitive") {
				t.Fatal(err, calls)
			}
		})
	}
}

func TestEditDraftAmbiguousResponses(t *testing.T) {
	for _, body := range []string{"", `{"id":"wrong","status":"submitted","revision":2}`, `{"id":"` + editID + `","status":"submitted","revision":1}`, `{"id":"` + editID + `","status":"converted","revision":2}`, strings.Repeat("x", (2<<20)+1), "transport-failure"} {
		c, _, _ := fixture(t)
		connect(t, c)
		calls := 0
		c.http.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if body == "transport-failure" {
				return nil, errors.New("sensitive token")
			}
			rec := httptest.NewRecorder()
			io.WriteString(rec, body)
			return rec.Result(), nil
		})
		name := "updated"
		_, err := c.EditDraft(context.Background(), editID, DraftEdit{ExpectedRevision: 1, Name: &name})
		if err == nil || calls != 1 || strings.Contains(err.Error(), "sensitive") {
			t.Fatal(err, calls)
		}
	}
}

func TestEditDraftRefreshBeforeSending(t *testing.T) {
	c, _, _ := fixture(t)
	connect(t, c)
	c.rec.AccessExpires = time.Now()
	original := c.http.Transport
	patches, refreshes := 0, 0
	c.http.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth/token/" {
			refreshes++
			return original.RoundTrip(r)
		}
		patches++
		if r.Method != "PATCH" || refreshes != 1 || r.Header.Get("Authorization") != "Bearer "+c.rec.AccessToken {
			t.Fatal("refresh must precede edit")
		}
		rec := httptest.NewRecorder()
		fmt.Fprintf(rec, `{"id":%q,"status":"submitted","revision":2}`, editID)
		return rec.Result(), nil
	})
	name := "updated"
	_, err := c.EditDraft(context.Background(), editID, DraftEdit{ExpectedRevision: 1, Name: &name})
	if err != nil || refreshes != 1 || patches != 1 {
		t.Fatal(err, refreshes, patches)
	}
}
