package server

import (
	"strings"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
)

// RetryMatch selects captured blocked rows that share a placeholder (and
// optionally a host). Used after the dashboard sets a secret from a log row.
type RetryMatch struct {
	Placeholder string `json:"placeholder"`
	Host        string `json:"host"`
}

// RetryResult is one sequential replay outcome.
type RetryResult struct {
	ID     string `json:"id"`
	Status int    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// matchingRetryable returns retryable blocked records that carry placeholder,
// oldest first. recs is newest-first (Store.Records). host "" or "*" matches
// any host; otherwise the host must match (case-insensitive).
func matchingRetryable(recs []store.Record, placeholder, host string) []store.Record {
	if placeholder == "" {
		return nil
	}
	anyHost := host == "" || host == "*"
	var out []store.Record
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		if !r.Retryable || r.Action != string(model.ActionBlock) || r.Capture == nil {
			continue
		}
		if !anyHost && !strings.EqualFold(r.Host, host) {
			continue
		}
		if !recordHasPlaceholder(r, placeholder) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func recordHasPlaceholder(r store.Record, placeholder string) bool {
	for _, f := range r.Findings {
		if f.Kind == string(model.FindingPlaceholder) && f.Token == placeholder {
			return true
		}
	}
	return false
}

// matchingRetryablePattern returns retryable blocked records that carry
// patternID, oldest first. host "" or "*" matches any host.
func matchingRetryablePattern(recs []store.Record, patternID, host string) []store.Record {
	if patternID == "" {
		return nil
	}
	anyHost := host == "" || host == "*"
	var out []store.Record
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		if !r.Retryable || r.Action != string(model.ActionBlock) || r.Capture == nil {
			continue
		}
		if !anyHost && !strings.EqualFold(r.Host, host) {
			continue
		}
		if !recordHasPattern(r, patternID) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func recordHasPattern(r store.Record, patternID string) bool {
	for _, f := range r.Findings {
		if f.Kind == string(model.FindingPatternSecret) && f.PatternID == patternID {
			return true
		}
	}
	return false
}

func reasonStrings(rs []model.Reason) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}
