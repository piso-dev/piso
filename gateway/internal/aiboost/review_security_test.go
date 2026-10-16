package aiboost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewFailuresNeverRetryOrLeak(t *testing.T) {
	for _, status := range []int{401, 403, 404, 409, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c, _ := reviewFixture(t)
			posts := 0
			c.http.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				if r.Method == "POST" {
					posts++
					var d Decision
					if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.ExpectedRevision != 1 {
						t.Fatal("changed displayed revision", err)
					}
					w.WriteHeader(status)
					io.WriteString(w, "sensitive upstream body")
				} else {
					io.WriteString(w, `{"id":"11111111-1111-1111-1111-111111111111","revision":99}`)
				}
				return w.Result(), nil
			})
			_, err := c.Decide(context.Background(), "11111111-1111-1111-1111-111111111111", "reject", Decision{ExpectedRevision: 1})
			var api *APIError
			if !errors.As(err, &api) || api.Status != status || strings.Contains(err.Error(), "sensitive") || posts != 1 {
				t.Fatal(posts, err)
			}
		})
	}
}
func TestDecisionRejectsRedirect(t *testing.T) {
	c, _ := reviewFixture(t)
	calls := 0
	c.http.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		w := httptest.NewRecorder()
		if r.Method == "GET" {
			io.WriteString(w, `{}`)
		} else {
			w.Header().Set("Location", "https://attacker.example/")
			w.WriteHeader(307)
		}
		return w.Result(), nil
	})
	_, err := c.Decide(context.Background(), "11111111-1111-1111-1111-111111111111", "approve", Decision{ExpectedRevision: 1})
	if err == nil || calls != 1 {
		t.Fatal("followed decision redirect", calls, err)
	}
}
