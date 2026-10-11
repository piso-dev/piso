package aiboost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"unicode/utf8"
)

// DraftEdit preserves omitted fields. Content replaces the entire content
// object; a non-nil empty SuggestedTags clears tags. AI Boost atomically checks
// the grant, submitted status and expected revision before creating a revision.
type DraftEdit struct {
	ExpectedRevision int       `json:"expectedRevision"`
	ChangeSummary    string    `json:"changeSummary,omitempty"`
	Name             *string   `json:"name,omitempty"`
	Description      *string   `json:"description,omitempty"`
	Content          *Content  `json:"content,omitempty"`
	SuggestedTags    *[]string `json:"suggestedTags,omitempty"`
	Rationale        *string   `json:"rationale,omitempty"`
	BaseVersionID    *string   `json:"baseVersionId,omitempty"`
	Version          *string   `json:"version,omitempty"`
	Changelog        *string   `json:"changelog,omitempty"`
}

func (e DraftEdit) Validate(id string) error {
	if !uuid.MatchString(id) {
		return errors.New("AI Boost draft UUID required")
	}
	if e.ExpectedRevision < 1 {
		return errors.New("expectedRevision must be a positive integer")
	}
	if !utf8.ValidString(e.ChangeSummary) || utf8.RuneCountInString(e.ChangeSummary) > 2000 {
		return errors.New("changeSummary must be at most 2000 characters")
	}
	versionFields := e.BaseVersionID != nil || e.Version != nil || e.Changelog != nil
	if versionFields && (e.Name != nil || e.Description != nil || e.SuggestedTags != nil) {
		return errors.New("cannot mix new-booster and version-draft fields")
	}
	if e.BaseVersionID != nil && !uuid.MatchString(*e.BaseVersionID) {
		return errors.New("baseVersionId must be a UUID")
	}
	if e.Version != nil {
		if err := validateVersion(*e.Version); err != nil {
			return err
		}
	}
	if e.Changelog != nil && !validVersionText(*e.Changelog) {
		return errors.New("changelog must be nonblank and at most 10000 characters")
	}
	if !versionFields && e.Name == nil && e.Description == nil && e.Content == nil && e.SuggestedTags == nil && e.Rationale == nil {
		return errors.New("provide at least one draft payload field")
	}
	// Reuse submission bounds without reading or replacing omitted fields.
	d := DraftPayload{Name: "valid", Description: "valid", Rationale: "valid", Content: Content{Format: "markdown", Body: "valid"}}
	if e.Name != nil {
		d.Name = *e.Name
	}
	if e.Description != nil {
		d.Description = *e.Description
	}
	if e.Content != nil {
		d.Content = *e.Content
	}
	if e.SuggestedTags != nil {
		if *e.SuggestedTags == nil {
			return errors.New("suggestedTags must be an array, not null")
		}
		d.SuggestedTags = *e.SuggestedTags
	}
	if e.Rationale != nil {
		d.Rationale = *e.Rationale
	}
	return d.validatePayload()
}

// EditDraft never auto-retries or advances expectedRevision. An ambiguous
// failure must be reconciled against the current draft before another edit.
func (c *Client) EditDraft(ctx context.Context, id string, edit DraftEdit) (Draft, error) {
	if err := edit.Validate(id); err != nil {
		return Draft{}, err
	}
	body, err := json.Marshal(edit)
	if err != nil {
		return Draft{}, err
	}
	raw, err := c.request(ctx, http.MethodPatch, "/api/drafts/"+id+"/", body, "")
	if err != nil {
		return Draft{}, err
	}
	var out Draft
	if json.Unmarshal(raw, &out) != nil || out.ID != id || out.Status != "submitted" || out.Revision <= 1 || out.Revision-1 != edit.ExpectedRevision {
		return Draft{}, errors.New("invalid AI Boost draft edit response")
	}
	return out, nil
}
