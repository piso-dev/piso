package aiboost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"
)

var ErrReviewInput = errors.New("invalid AI Boost review parameters")

type DraftPage struct {
	Items          []Draft `json:"items"`
	NextCursor     string  `json:"nextCursor,omitempty"`
	PreviousCursor string  `json:"previousCursor,omitempty"`
}

func reviewCursor(link *string) string {
	if link == nil {
		return ""
	}
	u, err := url.Parse(*link)
	if err != nil {
		return ""
	}
	cursor := u.Query().Get("cursor")
	if len(cursor) > 4096 {
		return ""
	}
	return cursor
}
func (c *Client) Drafts(ctx context.Context, status, cursor string) (DraftPage, error) {
	if len(cursor) > 4096 || (status != "" && !ValidDraftStatus(status)) {
		return DraftPage{}, ErrReviewInput
	}
	q := url.Values{"status": {status}, "cursor": {cursor}}
	raw, err := c.request(ctx, "GET", "/api/drafts/?"+q.Encode(), nil, "")
	if err != nil {
		return DraftPage{}, err
	}
	var page struct {
		Items    []Draft `json:"items"`
		Next     *string `json:"next"`
		Previous *string `json:"previous"`
	}
	if json.Unmarshal(raw, &page) != nil {
		return DraftPage{}, errors.New("invalid AI Boost draft page")
	}
	if page.Items == nil {
		page.Items = []Draft{}
	}
	return DraftPage{page.Items, reviewCursor(page.Next), reviewCursor(page.Previous)}, nil
}
func (c *Client) GetDraft(ctx context.Context, id string) (Draft, error) {
	if !uuid.MatchString(id) {
		return Draft{}, ErrReviewInput
	}
	raw, err := c.request(ctx, "GET", "/api/drafts/"+id+"/", nil, "")
	if err != nil {
		return Draft{}, err
	}
	return decodeDraft(raw, id)
}
func decodeDraft(raw []byte, id string) (Draft, error) {
	var out Draft
	if json.Unmarshal(raw, &out) != nil || out.ID == "" || (id != "" && out.ID != id) || out.Revision < 1 || out.Content.Format != "markdown" {
		return Draft{}, errors.New("invalid AI Boost draft")
	}
	return out, nil
}

type Decision struct {
	ExpectedRevision int    `json:"expectedRevision"`
	Feedback         string `json:"feedback"`
}

// ValidDraftStatuses is the upstream draft status vocabulary, including the
// terminal withdrawn state. Callers validate against this one list so the client
// and the MCP schema cannot drift.
var ValidDraftStatuses = []string{"submitted", "converted", "rejected", "withdrawn"}

func ValidDraftStatus(status string) bool {
	for _, s := range ValidDraftStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// requestOnce performs one gateway-credential call and never replays it, even
// after a 401. Mutations (decisions, withdrawals) and revision-locked writes use
// this path: an ambiguous failure may have committed, so the caller re-reads
// instead of retrying blind.
func (c *Client) requestOnce(ctx context.Context, method, path string, body []byte, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec.Status != "ok" {
		return nil, ErrReconnect
	}
	if !time.Now().Add(60 * time.Second).Before(c.rec.AccessExpires) {
		if err := c.refresh(ctx); err != nil {
			return nil, err
		}
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.rec.BaseURL+path, reader)
	if err != nil {
		return nil, ErrReviewInput
	}
	req.Header.Set("Authorization", "Bearer "+c.rec.AccessToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("AI Boost review unavailable; refresh the draft before retrying")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil || len(raw) > 2<<20 {
		return nil, errors.New("invalid AI Boost response")
	}
	return raw, nil
}

// Withdrawal retracts a submitted draft. Exactly one of ID or IdempotencyKey is
// set: the id form pins ExpectedRevision, and the key form resolves whatever is
// still undecided under the submission key the caller already holds.
type Withdrawal struct {
	ID               string
	IdempotencyKey   string
	ExpectedRevision int
	Reason           string
}

func (w Withdrawal) validate() (string, error) {
	if err := w.Validate(); err != nil {
		return "", err
	}
	if w.ID != "" {
		return "/api/drafts/" + w.ID + "/withdraw/", nil
	}
	return "/api/drafts/withdraw/", nil
}

// Validate reports whether the withdrawal is addressable under the upstream
// contract. Exactly one of ID or IdempotencyKey may be set: the id form pins
// ExpectedRevision, and the key form must not send one.
func (w Withdrawal) Validate() error {
	if !utf8.ValidString(w.Reason) || utf8.RuneCountInString(w.Reason) > 10000 {
		return errors.New("reason must be valid UTF-8 of at most 10000 characters")
	}
	switch {
	case w.ID != "" && w.IdempotencyKey != "":
		return errors.New("pass exactly one of id or idempotencyKey")
	case w.ID != "":
		if !uuid.MatchString(w.ID) {
			return errors.New("draft UUID required")
		}
		if w.ExpectedRevision < 1 {
			return errors.New("expectedRevision is required when withdrawing by draft id")
		}
		return nil
	case w.IdempotencyKey != "":
		if w.ExpectedRevision != 0 {
			return errors.New("omit expectedRevision when withdrawing by idempotencyKey")
		}
		return validKey(w.IdempotencyKey)
	default:
		return errors.New("pass exactly one of id or idempotencyKey")
	}
}

// Withdraw is the author's own recovery path. It never approves or publishes and
// is never replayed automatically: after a 409 or an ambiguous failure the caller
// re-reads the draft with GetDraft before deciding what to do next.
func (c *Client) Withdraw(ctx context.Context, w Withdrawal) (Draft, error) {
	path, err := w.validate()
	if err != nil {
		return Draft{}, fmt.Errorf("%w: %v", ErrReviewInput, err)
	}
	payload := map[string]any{"reason": w.Reason}
	if w.ID != "" {
		payload["expectedRevision"] = w.ExpectedRevision
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Draft{}, ErrReviewInput
	}
	raw, err := c.requestOnce(ctx, http.MethodPost, path, body, w.IdempotencyKey)
	if err != nil {
		return Draft{}, err
	}
	return decodeDraft(raw, w.ID)
}

// Decide is for host dashboard handlers only, never worker MCP. It uses the
// gateway OAuth credential, refreshing before sending, but never replays a decision.
func (c *Client) Decide(ctx context.Context, id, action string, decision Decision) (Draft, error) {
	if !uuid.MatchString(id) || (action != "approve" && action != "reject") || decision.ExpectedRevision < 1 || !utf8.ValidString(decision.Feedback) || utf8.RuneCountInString(decision.Feedback) > 10000 {
		return Draft{}, ErrReviewInput
	}
	body, err := json.Marshal(decision)
	if err != nil {
		return Draft{}, ErrReviewInput
	}
	raw, err := c.requestOnce(ctx, http.MethodPost, "/api/drafts/"+id+"/"+action+"/", body, "")
	if err != nil {
		return Draft{}, err
	}
	return decodeDraft(raw, id)
}
