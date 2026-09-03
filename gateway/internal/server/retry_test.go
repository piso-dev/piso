package server

import (
	"testing"

	"piso/gateway/internal/store"
)

func TestMatchingRetryableOldestFirst(t *testing.T) {
	recs := []store.Record{
		{ID: "new", Action: "block", Retryable: true, Host: "api.example.com", Capture: &store.Capture{},
			Findings: []store.Finding{{Kind: "placeholder", Token: "piso_x_aaa"}}},
		{ID: "old", Action: "block", Retryable: true, Host: "api.example.com", Capture: &store.Capture{},
			Findings: []store.Finding{{Kind: "placeholder", Token: "piso_x_aaa"}}},
		{ID: "other-host", Action: "block", Retryable: true, Host: "other.test", Capture: &store.Capture{},
			Findings: []store.Finding{{Kind: "placeholder", Token: "piso_x_aaa"}}},
		{ID: "other-tok", Action: "block", Retryable: true, Host: "api.example.com", Capture: &store.Capture{},
			Findings: []store.Finding{{Kind: "placeholder", Token: "piso_y_bbb"}}},
		{ID: "allowed", Action: "allow", Retryable: false, Host: "api.example.com",
			Findings: []store.Finding{{Kind: "placeholder", Token: "piso_x_aaa"}}},
	}
	// Store.Records is newest-first; "new" is index 0.
	got := matchingRetryable(recs, "piso_x_aaa", "api.example.com")
	if len(got) != 2 || got[0].ID != "old" || got[1].ID != "new" {
		t.Fatalf("host-scoped order: %+v", idsOf(got))
	}
	star := matchingRetryable(recs, "piso_x_aaa", "*")
	if len(star) != 3 || star[0].ID != "other-host" {
		t.Fatalf("*-scoped: %+v", idsOf(star))
	}
}

func TestMatchingRetryablePatternOldestFirst(t *testing.T) {
	recs := []store.Record{
		{ID: "new", Action: "block", Retryable: true, Host: "cdn.example.com", Capture: &store.Capture{},
			Findings: []store.Finding{{Kind: "pattern", PatternID: "jwt"}}},
		{ID: "old", Action: "block", Retryable: true, Host: "cdn.example.com", Capture: &store.Capture{},
			Findings: []store.Finding{{Kind: "pattern", PatternID: "jwt"}}},
		{ID: "other-host", Action: "block", Retryable: true, Host: "other.test", Capture: &store.Capture{},
			Findings: []store.Finding{{Kind: "pattern", PatternID: "jwt"}}},
		{ID: "other-pat", Action: "block", Retryable: true, Host: "cdn.example.com", Capture: &store.Capture{},
			Findings: []store.Finding{{Kind: "pattern", PatternID: "openai-sk"}}},
		{ID: "allowed", Action: "allow", Retryable: false, Host: "cdn.example.com",
			Findings: []store.Finding{{Kind: "pattern", PatternID: "jwt"}}},
	}
	got := matchingRetryablePattern(recs, "jwt", "cdn.example.com")
	if len(got) != 2 || got[0].ID != "old" || got[1].ID != "new" {
		t.Fatalf("host-scoped order: %+v", idsOf(got))
	}
	star := matchingRetryablePattern(recs, "jwt", "*")
	if len(star) != 3 || star[0].ID != "other-host" {
		t.Fatalf("*-scoped: %+v", idsOf(star))
	}
}

func idsOf(recs []store.Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}
