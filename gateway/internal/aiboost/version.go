package aiboost

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// SuggestionReview preserves the provider's distinction between creating a
// booster and publishing a version. Publication remains a host-only decision.
type SuggestionReview struct {
	Title           string `json:"title"`
	Action          string `json:"action"`
	BoosterID       string `json:"boosterId,omitempty"`
	BoosterName     string `json:"boosterName,omitempty"`
	BaseVersionID   string `json:"baseVersionId,omitempty"`
	BaseVersion     string `json:"baseVersion,omitempty"`
	ProposedVersion string `json:"proposedVersion"`
}

type VersionDraft struct {
	BoosterID     string  `json:"boosterId"`
	BaseVersionID string  `json:"baseVersionId"`
	Version       string  `json:"version"`
	Content       Content `json:"content"`
	Changelog     string  `json:"changelog"`
	Rationale     string  `json:"rationale"`
}

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

func validateVersion(version string) error {
	match := versionPattern.FindStringSubmatch(version)
	if len(version) > 32 || match == nil {
		return errors.New("version must be strict SemVer of at most 32 characters")
	}
	for _, identifier := range strings.Split(match[4], ".") {
		if len(identifier) > 1 && identifier[0] == '0' && strings.Trim(identifier, "0123456789") == "" {
			return errors.New("numeric prerelease identifiers cannot have leading zeros")
		}
	}
	return nil
}

func validVersionText(s string) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= 10000
}

func (d VersionDraft) Validate(key string) error {
	if !uuid.MatchString(d.BoosterID) || !uuid.MatchString(d.BaseVersionID) {
		return errors.New("boosterId and baseVersionId must be UUIDs")
	}
	if err := validateVersion(d.Version); err != nil {
		return err
	}
	if !validVersionText(d.Changelog) {
		return errors.New("changelog must be nonblank and at most 10000 characters")
	}
	// Share the key, content and rationale bounds with new-booster submissions.
	return (Draft{Name: "valid", Description: "valid", Content: d.Content, Rationale: d.Rationale}).Validate(key)
}

// SuggestVersion uses the same idempotent submission endpoint as Suggest.
// AI Boost validates live membership, eligibility, base identity and precedence.
func (c *Client) SuggestVersion(ctx context.Context, draft VersionDraft, key string) (Suggestion, error) {
	if err := draft.Validate(key); err != nil {
		return Suggestion{}, err
	}
	body, err := json.Marshal(struct {
		Type string `json:"type"`
		VersionDraft
	}{"new_version", draft})
	if err != nil {
		return Suggestion{}, err
	}
	raw, err := c.request(ctx, "POST", "/api/booster-suggestions/", body, key)
	if err != nil {
		return Suggestion{}, err
	}
	var out Suggestion
	if json.Unmarshal(raw, &out) != nil || !uuid.MatchString(out.ID) || out.Revision < 1 || out.Type != "new_version" || out.BoosterID != draft.BoosterID {
		return Suggestion{}, errors.New("invalid AI Boost version suggestion")
	}
	return out, nil
}
