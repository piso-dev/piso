package aiboost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	if len(cursor) > 4096 || (status != "" && status != "submitted" && status != "converted" && status != "rejected") {
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
	if json.Unmarshal(raw, &out) != nil || out.ID != id || out.Revision < 1 || out.Content.Format != "markdown" {
		return Draft{}, errors.New("invalid AI Boost draft")
	}
	return out, nil
}

type Decision struct {
	ExpectedRevision int    `json:"expectedRevision"`
	Feedback         string `json:"feedback"`
}

// Decide is for host dashboard handlers only, never worker MCP. It uses the
// gateway OAuth credential, refreshing before sending, but never replays a decision.
func (c *Client) Decide(ctx context.Context, id, action string, decision Decision) (Draft, error) {
	if !uuid.MatchString(id) || (action != "approve" && action != "reject") || decision.ExpectedRevision < 1 || !utf8.ValidString(decision.Feedback) || utf8.RuneCountInString(decision.Feedback) > 10000 {
		return Draft{}, ErrReviewInput
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec.Status != "ok" {
		return Draft{}, ErrReconnect
	}
	if !time.Now().Add(60 * time.Second).Before(c.rec.AccessExpires) {
		if err := c.refresh(ctx); err != nil {
			return Draft{}, err
		}
	}
	body, _ := json.Marshal(decision)
	req, err := http.NewRequestWithContext(ctx, "POST", c.rec.BaseURL+"/api/drafts/"+id+"/"+action+"/", bytes.NewReader(body))
	if err != nil {
		return Draft{}, errors.New("invalid AI Boost review request")
	}
	req.Header.Set("Authorization", "Bearer "+c.rec.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Draft{}, errors.New("AI Boost review unavailable; refresh the draft before retrying")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Draft{}, &APIError{resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil || len(raw) > 2<<20 {
		return Draft{}, errors.New("invalid AI Boost review response")
	}
	return decodeDraft(raw, id)
}
