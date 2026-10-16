package proxy

import (
	"strconv"
	"testing"

	"piso/gateway/internal/model"
	"piso/gateway/internal/scanner"
)

// A placeholder repeated within one request (e.g. a token echoed in several
// non-transcript fields) must collapse to one log finding with a count.
func TestToStoreFindingsCollapsesRepeatedPlaceholders(t *testing.T) {
	scan := scanner.Result{}
	for i := 0; i < 50; i++ {
		scan.Placeholders = append(scan.Placeholders, model.Finding{
			Kind:     model.FindingPlaceholder,
			Token:    "piso_model_5ef1739b3cf1",
			Location: "body",
			Field:    "field" + strconv.Itoa(i),
		})
	}
	got := toStoreFindings(scan)
	if len(got) != 1 {
		t.Fatalf("want 1 collapsed finding, got %d: %+v", len(got), got)
	}
	if got[0].Count != 50 {
		t.Fatalf("want count 50, got %d", got[0].Count)
	}
	if got[0].Field != "field0" {
		t.Fatalf("want first field kept, got %q", got[0].Field)
	}
}

// The LLM transcript is scanned on every turn and is full of piso_-looking
// strings (shell function names, docs, test literals). None of them are
// actionable, so the log must not record them.
func TestToStoreFindingsDropsChatNoise(t *testing.T) {
	scan := scanner.Result{}
	for i := 0; i < 50; i++ {
		scan.Placeholders = append(scan.Placeholders, model.Finding{
			Kind:     model.FindingPlaceholder,
			Token:    "piso_model_5ef1739b3cf1",
			Location: "json-body",
			Field:    "messages[" + strconv.Itoa(i) + "].content",
		})
	}
	// Not a placeholder at all — a shell function name.
	scan.Placeholders = append(scan.Placeholders, model.Finding{
		Kind: model.FindingPlaceholder, Token: "piso_gateway_post",
		Location: "json-body", Field: "messages[52].content",
	})
	scan.Placeholders = append(scan.Placeholders, model.Finding{
		Kind: model.FindingPlaceholder, Token: "piso_telegra_x",
		Location: "json-body", Field: "messages[35].reasoning_content",
	})
	// The one actionable finding: a real credential site.
	scan.Placeholders = append(scan.Placeholders, model.Finding{
		Kind: model.FindingPlaceholder, Token: "piso_github_app_testxx",
		Location: "authorization", Field: "Authorization",
	})
	got := toStoreFindings(scan)
	if len(got) != 1 {
		t.Fatalf("want only the credential-site finding, got %d: %+v", len(got), got)
	}
	if got[0].Token != "piso_github_app_testxx" || got[0].Location != "authorization" {
		t.Fatalf("wrong finding kept: %+v", got[0])
	}
}

// Pattern hits in the transcript are conversation too, but a real-secret match
// blocks even inside the transcript and must stay.
func TestToStoreFindingsDropsChatPatternsKeepsChatRealSecrets(t *testing.T) {
	scan := scanner.Result{
		PatternHits: []model.Finding{{
			Kind: model.FindingPatternSecret, PatternID: "generic-bearer",
			Token: "Bearer sk-…", Location: "json-body", Field: "messages[0].content",
		}},
		RealSecrets: []model.Finding{{
			Kind: model.FindingRealSecret, SecretID: "s1",
			Token: "sk-a…cdef", Location: "json-body", Field: "messages[0].content",
		}},
	}
	got := toStoreFindings(scan)
	if len(got) != 1 {
		t.Fatalf("want only the real-secret finding, got %d: %+v", len(got), got)
	}
	if got[0].Kind != "real-secret" {
		t.Fatalf("wrong finding kept: %+v", got[0])
	}
}

// Distinct tokens in the same location must not merge together.
func TestToStoreFindingsKeepsDistinctTokens(t *testing.T) {
	scan := scanner.Result{Placeholders: []model.Finding{
		{Kind: model.FindingPlaceholder, Token: "piso_a_111111", Location: "authorization"},
		{Kind: model.FindingPlaceholder, Token: "piso_b_222222", Location: "authorization"},
		{Kind: model.FindingPlaceholder, Token: "piso_a_111111", Location: "authorization"},
	}}
	got := toStoreFindings(scan)
	if len(got) != 2 {
		t.Fatalf("want 2 findings, got %d: %+v", len(got), got)
	}
	if got[0].Count != 2 || got[1].Count != 1 {
		t.Fatalf("counts wrong: %+v", got)
	}
}

// Two long tokens sharing a name prefix are still distinct: the dedupe key
// uses the FULL token, and placeholders are not truncated for display.
func TestToStoreFindingsNoPrefixCollision(t *testing.T) {
	const prefix = "piso_model_"
	a := prefix + "aaaa1111aaaa1111"
	b := prefix + "bbbb2222bbbb2222"
	scan := scanner.Result{Placeholders: []model.Finding{
		{Kind: model.FindingPlaceholder, Token: a, Location: "authorization"},
		{Kind: model.FindingPlaceholder, Token: b, Location: "authorization"},
	}}
	got := toStoreFindings(scan)
	if len(got) != 2 {
		t.Fatalf("distinct tokens merged: %+v", got)
	}
	if got[0].Token != a || got[1].Token != b {
		t.Fatalf("placeholder display token altered: %+v", got)
	}
}

// displayToken: a placeholder is not a secret and its random suffix is what
// distinguishes similarly-named secrets, so it is kept whole. Sensitive
// pattern/real tokens stay bounded.
func TestDisplayToken(t *testing.T) {
	const ph = "piso_anthropic_abc123"
	if got := displayToken(model.FindingPlaceholder, ph); got != ph {
		t.Fatalf("placeholder must render whole, got %q", got)
	}
	long := "piso_github_app_" + "0123456789012345678901234567890123456789"
	got := displayToken(model.FindingPlaceholder, long)
	if got == long {
		t.Fatalf("absurdly long placeholder should be bounded: %q", got)
	}
	if len(got) > 48 {
		t.Fatalf("placeholder bound too loose: %d", len(got))
	}
	if got := displayToken(model.FindingPatternSecret, "Bearer sk-live-0123456789abcdef"); got != "Bearer sk-li…" {
		t.Fatalf("pattern token bound wrong: %q", got)
	}
	if got := displayToken(model.FindingRealSecret, "sk-live-0123456789abcdefg"); got != "sk-live-0123…" {
		t.Fatalf("real-secret token bound wrong: %q", got)
	}
	if got := displayToken(model.FindingPatternSecret, "short"); got != "short" {
		t.Fatalf("short token must be untouched: %q", got)
	}
}
