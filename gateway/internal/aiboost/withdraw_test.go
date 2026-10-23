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

const withdrawnFixture = `{"id":"11111111-1111-1111-1111-111111111111","status":"withdrawn","revision":1,"name":"Go","description":"d","content":{"format":"markdown","body":"# b"},"idempotencyKey":"misfire","decision":null,"result":null,"withdrawal":{"revision":1,"reason":"Wrong target.","withdrawnAt":"2026-01-01T00:00:00Z","connectionId":"c1"}}`

type withdrawCall struct {
	Method string
	Path   string
	Key    string
	Body   map[string]any
	Auth   string
}

func withdrawFixture(t *testing.T, statuses ...int) (*Client, *[]withdrawCall) {
	t.Helper()
	var calls []withdrawCall
	c, _, _ := fixture(t)
	connect(t, c)
	status := 200
	if len(statuses) > 0 {
		status = statuses[0]
	}
	c.http.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		call := withdrawCall{Method: r.Method, Path: r.URL.Path, Key: r.Header.Get("Idempotency-Key"), Auth: r.Header.Get("Authorization")}
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &call.Body)
		}
		calls = append(calls, call)
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json")
		if status != 200 {
			rec.WriteHeader(status)
			io.WriteString(rec, "sensitive upstream body")
			return rec.Result(), nil
		}
		io.WriteString(rec, withdrawnFixture)
		return rec.Result(), nil
	})
	return c, &calls
}

func TestWithdrawByIDPinsRevision(t *testing.T) {
	c, calls := withdrawFixture(t)
	out, err := c.Withdraw(context.Background(), Withdrawal{
		ID: "11111111-1111-1111-1111-111111111111", ExpectedRevision: 1, Reason: "Wrong target.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "withdrawn" || out.Withdrawal == nil || out.Withdrawal.Revision != 1 || out.Withdrawal.Reason != "Wrong target." {
		t.Fatal(out)
	}
	if out.IdempotencyKey != "misfire" {
		t.Fatal("idempotency key must survive decode", out.IdempotencyKey)
	}
	if len(*calls) != 1 {
		t.Fatal(*calls)
	}
	call := (*calls)[0]
	if call.Method != http.MethodPost || call.Path != "/api/drafts/11111111-1111-1111-1111-111111111111/withdraw/" {
		t.Fatal(call)
	}
	if call.Key != "" || call.Auth != "Bearer access-token-never-public" {
		t.Fatal(call)
	}
	if call.Body["expectedRevision"] != float64(1) || call.Body["reason"] != "Wrong target." {
		t.Fatal(call.Body)
	}
}

func TestWithdrawByKeyOmitsRevision(t *testing.T) {
	c, calls := withdrawFixture(t)
	out, err := c.Withdraw(context.Background(), Withdrawal{IdempotencyKey: "misfire", Reason: "Submitted in error."})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "withdrawn" || out.ID != "11111111-1111-1111-1111-111111111111" {
		t.Fatal(out)
	}
	call := (*calls)[0]
	if call.Path != "/api/drafts/withdraw/" || call.Key != "misfire" {
		t.Fatal(call)
	}
	// Upstream rejects a pinned revision on the key-addressed route with 400.
	if _, ok := call.Body["expectedRevision"]; ok || call.Body["reason"] != "Submitted in error." {
		t.Fatal(call.Body)
	}
}

func TestWithdrawValidatesInputWithoutCallingUpstream(t *testing.T) {
	c, calls := withdrawFixture(t)
	const id = "11111111-1111-1111-1111-111111111111"
	cases := map[string]Withdrawal{
		"neither form":           {Reason: "x"},
		"both forms":             {ID: id, ExpectedRevision: 1, IdempotencyKey: "k"},
		"bad uuid":               {ID: "../approve", ExpectedRevision: 1},
		"missing revision":       {ID: id},
		"revision with key form": {IdempotencyKey: "k", ExpectedRevision: 2},
		"blank key":              {IdempotencyKey: ""},
		"non printable key":      {IdempotencyKey: "bad\nkey"},
		"space in key":           {IdempotencyKey: "has space"},
		"reason too long":        {ID: id, ExpectedRevision: 1, Reason: strings.Repeat("x", 10001)},
	}
	for name, w := range cases {
		if _, err := c.Withdraw(context.Background(), w); !errors.Is(err, ErrReviewInput) {
			t.Fatal(name, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatal("invalid input reached upstream", *calls)
	}
}

func TestWithdrawNeverReplays(t *testing.T) {
	for _, status := range []int{401, 403, 409, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c, calls := withdrawFixture(t, status)
			_, err := c.Withdraw(context.Background(), Withdrawal{
				ID: "11111111-1111-1111-1111-111111111111", ExpectedRevision: 1, Reason: "x",
			})
			var api *APIError
			if !errors.As(err, &api) || api.Status != status {
				t.Fatal(err)
			}
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatal("leaked upstream body")
			}
			// A 401 may refresh and retry reads and submissions, never a mutation.
			if len(*calls) != 1 {
				t.Fatal("withdrawal was replayed", len(*calls))
			}
		})
	}
}

func TestWithdrawnStatusIsListable(t *testing.T) {
	c, _ := reviewFixture(t)
	if _, err := c.Drafts(context.Background(), "withdrawn", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Drafts(context.Background(), "pending", ""); !errors.Is(err, ErrReviewInput) {
		t.Fatal(err)
	}
	for _, status := range ValidDraftStatuses {
		if !ValidDraftStatus(status) {
			t.Fatal(status)
		}
	}
	if ValidDraftStatus("") || ValidDraftStatus("Withdrawn") {
		t.Fatal("empty or unknown statuses must not validate")
	}
}
