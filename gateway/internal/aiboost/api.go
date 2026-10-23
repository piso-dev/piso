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

type DraftPayload struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Content       Content  `json:"content"`
	SuggestedTags []string `json:"suggestedTags"`
	Rationale     string   `json:"rationale"`
}
type Draft struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Revision int    `json:"revision"`
	DraftPayload
	Type           string           `json:"type,omitempty"`
	BoosterID      string           `json:"boosterId,omitempty"`
	BaseVersionID  string           `json:"baseVersionId,omitempty"`
	Version        string           `json:"version,omitempty"`
	Changelog      string           `json:"changelog,omitempty"`
	Review         *DraftReview     `json:"review,omitempty"`
	CreatedAt      string           `json:"createdAt,omitempty"`
	UpdatedAt      string           `json:"updatedAt,omitempty"`
	IdempotencyKey string           `json:"idempotencyKey,omitempty"`
	Decision       json.RawMessage  `json:"decision,omitempty"`
	Withdrawal     *DraftWithdrawal `json:"withdrawal,omitempty"`
	Result         json.RawMessage  `json:"result,omitempty"`
	Destination    struct {
		TagID      string `json:"tagId"`
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
	} `json:"destination"`
}

// DraftWithdrawal records how a draft was retracted. A withdrawal is not a
// review decision: it needs drafts:write, publishes nothing, and keeps the
// draft's revisions and comments intact.
type DraftWithdrawal struct {
	Revision     int    `json:"revision"`
	Reason       string `json:"reason"`
	WithdrawnAt  string `json:"withdrawnAt,omitempty"`
	ConnectionID string `json:"connectionId,omitempty"`
}

// validKey reports whether key satisfies the upstream Idempotency-Key contract.
func validKey(key string) error {
	if len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
		return errors.New("stable Idempotency-Key required (1–128 printable ASCII characters)")
	}
	for _, r := range key {
		if r < 33 || r > 126 {
			return errors.New("Idempotency-Key must be printable ASCII without spaces")
		}
	}
	return nil
}

// Validate matches the delegated AI Boost draft input contract.
func (d DraftPayload) Validate(key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	return d.validatePayload()
}

func (d DraftPayload) validatePayload() error {
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

// Draft never approves or publishes. The caller persists a UUID with the
// draft, reuses it on retries, and mints a new one for a changed payload.
func (c *Client) Draft(ctx context.Context, draft DraftPayload, key string) (Draft, error) {
	if err := draft.Validate(key); err != nil {
		return Draft{}, err
	}
	if draft.SuggestedTags == nil {
		draft.SuggestedTags = []string{}
	}
	body, err := json.Marshal(draft)
	if err != nil {
		return Draft{}, err
	}
	raw, err := c.request(ctx, "POST", "/api/drafts/", body, key)
	if err != nil {
		return Draft{}, err
	}
	var out Draft
	if json.Unmarshal(raw, &out) != nil || out.ID == "" {
		return Draft{}, errors.New("invalid AI Boost draft")
	}
	return out, nil
}
