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
// AI Boost enforces the connected library credential on every read.
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

// Validate matches the delegated AI Boost suggestion input contract.
func (d Draft) Validate(key string) error {
	if len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
		return errors.New("stable Idempotency-Key required (1–128 printable ASCII characters)")
	}
	for _, r := range key {
		if r < 33 || r > 126 {
			return errors.New("Idempotency-Key must be printable ASCII without spaces")
		}
	}
	for _, field := range []struct {
		value string
		max   int
	}{{d.Name, 255}, {d.Description, 10000}, {d.Rationale, 10000}} {
		if strings.TrimSpace(field.value) == "" || utf8.RuneCountInString(field.value) > field.max || !utf8.ValidString(field.value) {
			return errors.New("name, description and rationale required within AI Boost size limits")
		}
	}
	if d.Content.Format != "markdown" || strings.TrimSpace(d.Content.Body) == "" || len(d.Content.Body) > 100000 || !utf8.ValidString(d.Content.Body) {
		return errors.New("content must be nonblank Markdown of at most 100000 UTF-8 bytes")
	}
	if len(d.SuggestedTags) > 30 {
		return errors.New("at most 30 suggestedTags allowed")
	}
	for _, tag := range d.SuggestedTags {
		if strings.TrimSpace(tag) == "" || utf8.RuneCountInString(tag) > 100 || !utf8.ValidString(tag) {
			return errors.New("suggestedTags must be nonblank strings of at most 100 characters")
		}
	}
	return nil
}

// Suggest never approves or publishes. The caller persists a UUID with the
// draft, reuses it on retries, and mints a new one for a changed payload.
func (c *Client) Suggest(ctx context.Context, draft Draft, key string) (Suggestion, error) {
	if err := draft.Validate(key); err != nil {
		return Suggestion{}, err
	}
	if draft.SuggestedTags == nil {
		draft.SuggestedTags = []string{}
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
