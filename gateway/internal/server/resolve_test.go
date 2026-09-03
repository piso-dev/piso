package server

import "testing"

func TestSlugToken(t *testing.T) {
	if got := slugToken("ANTHROPIC_API_KEY"); got != "anthropic_api_key" {
		t.Fatalf("got %q", got)
	}
	if got := slugToken("!!!"); got != "secret" {
		t.Fatalf("empty slug: %q", got)
	}
}
