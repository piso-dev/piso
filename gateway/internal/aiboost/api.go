package aiboost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

type Content struct {
	Format string `json:"format"`
	Body   string `json:"body"`
}
type Booster struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Version     string   `json:"version"`
	VersionID   string   `json:"versionId"`
	UpdatedAt   string   `json:"updatedAt"`
	Content     Content  `json:"content,omitempty"`
}
type Page struct {
	Items    []Booster `json:"items"`
	Next     *string   `json:"next"`
	Previous *string   `json:"previous"`
}

// List accepts a cursor, not an arbitrary pagination URL. Never forward a
// Bearer token to a next/previous URL received from an upstream response.
func (c *Client) List(ctx context.Context, query, cursor string) (Page, error) {
	if utf8.RuneCountInString(query) > 500 || len(cursor) > 4096 {
		return Page{}, errors.New("AI Boost search parameters too long")
	}
	q := url.Values{"q": {query}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	raw, err := c.request(ctx, "GET", "/api/library/boosters/?"+q.Encode(), nil, "")
	if err != nil {
		return Page{}, err
	}
	var page Page
	if json.Unmarshal(raw, &page) != nil {
		return Page{}, errors.New("invalid AI Boost page")
	}
	return page, nil
}

var uuid = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func (c *Client) Get(ctx context.Context, id string) (Booster, error) {
	if !uuid.MatchString(id) {
		return Booster{}, errors.New("invalid booster ID")
	}
	raw, err := c.request(ctx, "GET", "/api/library/boosters/"+id+"/", nil, "")
	if err != nil {
		return Booster{}, err
	}
	var b Booster
	if json.Unmarshal(raw, &b) != nil || b.ID != id || b.Content.Format != "markdown" {
		return Booster{}, errors.New("invalid AI Boost booster")
	}
	return b, nil
}

// Excerpt hashes the full upstream Markdown, but returns at most 8,000 runes.
// Authorization and worker collection filtering must happen before calling Get.
type Excerpt struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Text      string `json:"excerpt"`
	SHA256    string `json:"sha256"`
	Truncated bool   `json:"truncated"`
}

func (b Booster) Excerpt() Excerpt {
	sum := sha256.Sum256([]byte(b.Content.Body))
	runes := []rune(b.Content.Body)
	truncated := len(runes) > 8000
	if truncated {
		runes = runes[:8000]
	}
	return Excerpt{b.ID, b.Name, b.Version, string(runes), hex.EncodeToString(sum[:]), truncated}
}

type Draft struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Content       Content  `json:"content"`
	SuggestedTags []string `json:"suggestedTags"`
	Rationale     string   `json:"rationale"`
}
type Suggestion struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Revision int    `json:"revision"`
	Draft
	Destination struct {
		TagID      string `json:"tagId"`
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
	} `json:"destination"`
}

// Suggest never approves or publishes. The caller persists a UUID with the
// draft, reuses it on retries, and mints a new one for a changed payload.
func (c *Client) Suggest(ctx context.Context, draft Draft, key string) (Suggestion, error) {
	if len(key) < 1 || len(key) > 128 || strings.ContainsAny(key, "\r\n") {
		return Suggestion{}, errors.New("stable Idempotency-Key required (1–128 characters)")
	}
	if draft.Content.Format != "markdown" || len(draft.Content.Body) > 100000 || !utf8.ValidString(draft.Content.Body) {
		return Suggestion{}, errors.New("suggestion must contain Markdown of at most 100000 UTF-8 bytes")
	}
	body, err := json.Marshal(draft)
	if err != nil {
		return Suggestion{}, err
	}
	raw, err := c.request(ctx, "POST", "/api/booster-suggestions/", body, key)
	if err != nil {
		return Suggestion{}, err
	}
	var out Suggestion
	if json.Unmarshal(raw, &out) != nil || out.ID == "" {
		return Suggestion{}, errors.New("invalid AI Boost suggestion")
	}
	return out, nil
}
