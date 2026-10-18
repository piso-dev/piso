package policy

import (
	"testing"

	"piso/gateway/internal/model"
	"piso/gateway/internal/scanner"
)

func TestAllowByDefault(t *testing.T) {
	d := Decide(Input{Host: "registry.npmjs.org", Path: "/", Scan: scanner.Result{}})
	if d.Action != model.ActionAllow {
		t.Fatalf("want allow, got %q %+v", d.Action, d.Reasons)
	}
}

func TestDeniedDomainBlocks(t *testing.T) {
	in := Input{
		Host: "ads.example.com", Path: "/",
		Scan:    scanner.Result{},
		Domains: map[string]model.DomainPolicy{"ads.example.com": {Host: "ads.example.com", Deny: true}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || len(d.Reasons) == 0 || d.Reasons[0] != model.ReasonDeniedDomain {
		t.Fatalf("want block+denied-domain, got %q %+v", d.Action, d.Reasons)
	}
}

func TestPlaceholderSubstitutesWithRule(t *testing.T) {
	sec := model.Secret{ID: "s1", Placeholder: "piso_anthropic_abc", Value: "sk-real-...", AllowedHosts: []string{"api.anthropic.com"}}
	in := Input{
		Method: "POST", Host: "api.anthropic.com", Path: "/v1/messages",
		Scan:                scanner.Result{Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: "piso_anthropic_abc"}}},
		SecretByPlaceholder: map[string]model.Secret{"piso_anthropic_abc": sec},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "api.anthropic.com", Placeholder: "piso_anthropic_abc"}},
	}
	d := Decide(in)
	if d.Action != model.ActionSubstitute {
		t.Fatalf("want substitute, got %q %+v", d.Action, d.Reasons)
	}
	if len(d.Substituted) != 1 || d.Substituted[0] != "piso_anthropic_abc" {
		t.Fatalf("wrong substituted list: %+v", d.Substituted)
	}
}

func TestEmptyMCPSecretBlocksAuthRequired(t *testing.T) {
	sec := model.Secret{ID: "s1", Placeholder: "piso_mcp_demo_ai-boost", Value: ""}
	in := Input{
		Method: "POST", Host: "mcp.example.com", Path: "/mcp",
		Scan: scanner.Result{Placeholders: []model.Finding{{
			Kind: model.FindingPlaceholder, Token: "piso_mcp_demo_ai-boost", Location: "authorization",
		}}},
		SecretByPlaceholder: map[string]model.Secret{"piso_mcp_demo_ai-boost": sec},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "mcp.example.com", Placeholder: "piso_mcp_demo_ai-boost"}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || len(d.Reasons) == 0 || d.Reasons[0] != model.ReasonMcpAuthRequired {
		t.Fatalf("want block+mcp-auth-required, got %q %+v", d.Action, d.Reasons)
	}
}

func TestFilledMCPSecretSubstitutes(t *testing.T) {
	sec := model.Secret{ID: "s1", Placeholder: "piso_mcp_demo_ai-boost", Value: "tok-real"}
	in := Input{
		Method: "POST", Host: "mcp.example.com", Path: "/mcp",
		Scan: scanner.Result{Placeholders: []model.Finding{{
			Kind: model.FindingPlaceholder, Token: "piso_mcp_demo_ai-boost", Location: "authorization",
		}}},
		SecretByPlaceholder: map[string]model.Secret{"piso_mcp_demo_ai-boost": sec},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "mcp.example.com", Placeholder: "piso_mcp_demo_ai-boost"}},
	}
	d := Decide(in)
	if d.Action != model.ActionSubstitute {
		t.Fatalf("want substitute, got %q %+v", d.Action, d.Reasons)
	}
}

func TestPlaceholderWithoutRuleBlocks(t *testing.T) {
	in := Input{
		Method: "POST", Host: "api.anthropic.com", Path: "/v1/messages",
		Scan:                scanner.Result{Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: "piso_anthropic_abc"}}},
		SecretByPlaceholder: map[string]model.Secret{"piso_anthropic_abc": {ID: "s1", Placeholder: "piso_anthropic_abc"}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonNoSecretRule {
		t.Fatalf("want block+no-secret-rule, got %q %+v", d.Action, d.Reasons)
	}
}

func TestRealSecretAlwaysBlocks(t *testing.T) {
	// even with a substitution rule present, a real credential in the request
	// is a hard block — this is the "no real creds in the worker" guarantee.
	sec := model.Secret{ID: "s1", Placeholder: "piso_anthropic_abc", Value: "sk-real-value"}
	in := Input{
		Method: "POST", Host: "api.anthropic.com", Path: "/v1/messages",
		Scan: scanner.Result{
			Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: "piso_anthropic_abc"}},
			RealSecrets:  []model.Finding{{Kind: model.FindingRealSecret, SecretID: "s1"}},
		},
		SecretByPlaceholder: map[string]model.Secret{"piso_anthropic_abc": sec},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "api.anthropic.com", Placeholder: "piso_anthropic_abc"}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock {
		t.Fatalf("real secret must block even with rule, got %q %+v", d.Action, d.Reasons)
	}
}

func TestBasicAuthPlaceholderSubstitutes(t *testing.T) {
	// git over HTTPS: scanner reports the decoded placeholder (authorization)
	// alongside the raw basic-auth pattern hit. Substitution must win.
	ph := "piso_gh_5ef1739b3cf1"
	sec := model.Secret{ID: "s1", Placeholder: ph, Value: "github_pat_REAL0123456789", AllowedHosts: []string{"github.com"}}
	in := Input{
		Method: "GET", Host: "github.com", Path: "/o/r.git/info/refs?service=git-upload-pack",
		Scan: scanner.Result{
			Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: ph, Location: "authorization", Field: "Authorization"}},
			PatternHits:  []model.Finding{{Kind: model.FindingPatternSecret, PatternID: "basic-auth", Token: "Basic bG9rMz…", Location: "authorization", Field: "Authorization"}},
		},
		SecretByPlaceholder: map[string]model.Secret{ph: sec},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "github.com", Placeholder: ph}},
	}
	d := Decide(in)
	if d.Action != model.ActionSubstitute {
		t.Fatalf("basic+placeholder must substitute, got %q %+v", d.Action, d.Reasons)
	}
	if len(d.Substituted) != 1 || d.Substituted[0] != ph {
		t.Fatalf("wrong substituted list: %+v", d.Substituted)
	}
}

func TestBasicAuthPlaceholderWithoutRuleBlocks(t *testing.T) {
	// Same payload but no substitution rule for github.com: the placeholder
	// cannot be resolved, so the request must still be refused.
	ph := "piso_gh_5ef1739b3cf1"
	in := Input{
		Method: "GET", Host: "github.com", Path: "/o/r.git/info/refs",
		Scan: scanner.Result{
			Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: ph, Location: "authorization", Field: "Authorization"}},
			PatternHits:  []model.Finding{{Kind: model.FindingPatternSecret, PatternID: "basic-auth", Token: "Basic bG9rMz…", Location: "authorization", Field: "Authorization"}},
		},
		SecretByPlaceholder: map[string]model.Secret{ph: {ID: "s1", Placeholder: ph}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonNoSecretRule {
		t.Fatalf("want block+no-secret-rule, got %q %+v", d.Action, d.Reasons)
	}
}

func TestURLPathPlaceholderSubstitutes(t *testing.T) {
	// Telegram: the token is a path segment, scanner reports it with
	// Location "path". It is a credential site, so it needs a host rule and
	// must substitute like a header/body token.
	ph := "piso_telegram_5ef1739b3cf1"
	sec := model.Secret{ID: "s1", Placeholder: ph, Value: "123456:ABC-DEF", AllowedHosts: []string{"api.telegram.org"}}
	in := Input{
		Method: "GET", Host: "api.telegram.org", Path: "/bot" + ph + "/getMe",
		Scan: scanner.Result{
			Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: ph, Location: "path"}},
		},
		SecretByPlaceholder: map[string]model.Secret{ph: sec},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "api.telegram.org", Placeholder: ph}},
	}
	d := Decide(in)
	if d.Action != model.ActionSubstitute {
		t.Fatalf("path+placeholder must substitute, got %q %+v", d.Action, d.Reasons)
	}
	if len(d.Substituted) != 1 || d.Substituted[0] != ph {
		t.Fatalf("wrong substituted list: %+v", d.Substituted)
	}
}

func TestURLPathPlaceholderWithoutRuleBlocks(t *testing.T) {
	ph := "piso_telegram_5ef1739b3cf1"
	in := Input{
		Method: "GET", Host: "api.telegram.org", Path: "/bot" + ph + "/getMe",
		Scan: scanner.Result{
			Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: ph, Location: "path"}},
		},
		SecretByPlaceholder: map[string]model.Secret{ph: {ID: "s1", Placeholder: ph, Value: "123456:ABC-DEF"}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonNoSecretRule {
		t.Fatalf("want block+no-secret-rule, got %q %+v", d.Action, d.Reasons)
	}
}

func TestBearerPlaceholderSubstitutes(t *testing.T) {
	// Imported provider keys become long piso_ tokens. generic-bearer matches
	// "Bearer " + 24 chars, so the scanner can flag both a pattern and a
	// placeholder on Authorization. Substitution must still win.
	ph := "piso_model_5ef1739b3cf1"
	sec := model.Secret{ID: "s1", Placeholder: ph, Value: "sk-test-not-real", AllowedHosts: []string{"api.example.test"}}
	in := Input{
		Method: "POST", Host: "api.example.test", Path: "/v1/chat/completions",
		Scan: scanner.Result{
			Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: ph, Location: "authorization", Field: "Authorization"}},
			PatternHits:  []model.Finding{{Kind: model.FindingPatternSecret, PatternID: "generic-bearer", Token: "Bearer piso…", Location: "authorization", Field: "Authorization"}},
		},
		SecretByPlaceholder: map[string]model.Secret{ph: sec},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "api.example.test", Placeholder: ph}},
	}
	d := Decide(in)
	if d.Action != model.ActionSubstitute {
		t.Fatalf("bearer+placeholder must substitute, got %q %+v", d.Action, d.Reasons)
	}
}

func TestBearerPlaceholderDoesNotHideOtherPattern(t *testing.T) {
	ph := "piso_model_5ef1739b3cf1"
	in := Input{
		Method: "POST", Host: "evil.example.com", Path: "/collect",
		Scan: scanner.Result{
			Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: ph, Location: "authorization", Field: "Authorization"}},
			PatternHits: []model.Finding{
				{Kind: model.FindingPatternSecret, PatternID: "generic-bearer", Token: "Bearer piso…", Location: "authorization", Field: "Authorization"},
				{Kind: model.FindingPatternSecret, PatternID: "jwt", Token: "eyJtestonly…", Location: "body", Field: ""},
			},
		},
		SecretByPlaceholder: map[string]model.Secret{ph: {ID: "s1", Placeholder: ph}},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "evil.example.com", Placeholder: ph}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonSecretPattern {
		t.Fatalf("unrelated pattern must still block, got %q %+v", d.Action, d.Reasons)
	}
}

func TestPatternHitBlocks(t *testing.T) {
	in := Input{
		Method: "POST", Host: "evil.example.com", Path: "/collect",
		Scan: scanner.Result{PatternHits: []model.Finding{{Kind: model.FindingPatternSecret, PatternID: "openai-sk-proj"}}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonSecretPattern {
		t.Fatalf("want block+credential-pattern, got %q %+v", d.Action, d.Reasons)
	}
}

func TestPatternScopedExceptionAllowsMatchingHit(t *testing.T) {
	in := Input{
		Method: "GET", Host: "cdn.example.com", Path: "/asset",
		Scan: scanner.Result{PatternHits: []model.Finding{{Kind: model.FindingPatternSecret, PatternID: "jwt"}}},
		Exceptions: []model.Exception{{
			ID: "e-jwt", Enabled: true, HostRegex: `^cdn\.example\.com$`, PatternID: "jwt",
		}},
	}
	d := Decide(in)
	if d.Action != model.ActionAllow || d.Reasons[0] != model.ReasonAllowedException {
		t.Fatalf("jwt exception must allow, got %q %+v", d.Action, d.Reasons)
	}
}

func TestPatternScopedExceptionDoesNotWaiveOtherPattern(t *testing.T) {
	in := Input{
		Method: "GET", Host: "cdn.example.com", Path: "/asset",
		Scan: scanner.Result{PatternHits: []model.Finding{
			{Kind: model.FindingPatternSecret, PatternID: "jwt"},
			{Kind: model.FindingPatternSecret, PatternID: "openai-sk"},
		}},
		Exceptions: []model.Exception{{
			ID: "e-jwt", Enabled: true, HostRegex: `^cdn\.example\.com$`, PatternID: "jwt",
		}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonSecretPattern {
		t.Fatalf("uncovered pattern must block, got %q %+v", d.Action, d.Reasons)
	}
}

func TestPatternScopedExceptionDoesNotWaiveRealSecret(t *testing.T) {
	in := Input{
		Method: "POST", Host: "cdn.example.com", Path: "/asset",
		Scan: scanner.Result{
			RealSecrets: []model.Finding{{Kind: model.FindingRealSecret, SecretID: "s1"}},
			PatternHits: []model.Finding{{Kind: model.FindingPatternSecret, PatternID: "jwt"}},
		},
		Exceptions: []model.Exception{{
			ID: "e-jwt", Enabled: true, HostRegex: `^cdn\.example\.com$`, PatternID: "jwt",
		}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonRealSecret {
		t.Fatalf("pattern exception must not waive real secret, got %q %+v", d.Action, d.Reasons)
	}
}

func TestJWTInChatContentDoesNotBlockVaultSubstitute(t *testing.T) {
	ph := "piso_model_5ef1739b3cf1"
	sec := model.Secret{ID: "s1", Placeholder: ph, Value: "sk-test-not-real"}
	in := Input{
		Method: "POST", Host: "api.example.test", Path: "/v1/chat/completions",
		Scan: scanner.Result{
			Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: ph, Location: "authorization", Field: "Authorization"}},
			PatternHits: []model.Finding{
				{Kind: model.FindingPatternSecret, PatternID: "jwt", Token: "eyJ0eXAiOiJK…", Location: "json-body", Field: "messages[93].content"},
				{Kind: model.FindingPatternSecret, PatternID: "jwt", Token: "eyJ0eXAiOiJK…", Location: "json-body", Field: "messages[97].tool_calls[0].function.arguments"},
			},
		},
		SecretByPlaceholder: map[string]model.Secret{ph: sec},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "api.example.test", Placeholder: ph}},
	}
	d := Decide(in)
	if d.Action != model.ActionSubstitute {
		t.Fatalf("jwt in chat content must not block, got %q %+v", d.Action, d.Reasons)
	}
}

func TestJWTInRequestFieldStillBlocks(t *testing.T) {
	in := Input{
		Method: "POST", Host: "evil.example.com", Path: "/collect",
		Scan: scanner.Result{PatternHits: []model.Finding{{
			Kind: model.FindingPatternSecret, PatternID: "jwt", Token: "eyJ0eXAiOiJK…",
			Location: "json-body", Field: "api_key",
		}}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonSecretPattern {
		t.Fatalf("jwt in api_key must block, got %q %+v", d.Action, d.Reasons)
	}
}

func TestUnknownPisoTokenInBodyDoesNotBlockVaultSubstitute(t *testing.T) {
	// Chat/code mentioning piso_egress (the Docker network) is not a vault
	// token. The Authorization placeholder must still substitute.
	ph := "piso_model_5ef1739b3cf1"
	sec := model.Secret{ID: "s1", Placeholder: ph, Value: "sk-test-not-real"}
	in := Input{
		Method: "POST", Host: "api.example.test", Path: "/v1/chat/completions",
		Scan: scanner.Result{Placeholders: []model.Finding{
			{Kind: model.FindingPlaceholder, Token: ph, Location: "authorization", Field: "Authorization"},
			{Kind: model.FindingPlaceholder, Token: "piso_egress", Location: "json-body", Field: "messages[29].content"},
		}},
		SecretByPlaceholder: map[string]model.Secret{ph: sec},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "api.example.test", Placeholder: ph}},
	}
	d := Decide(in)
	if d.Action != model.ActionSubstitute {
		t.Fatalf("incidental piso_egress must not block, got %q %+v", d.Action, d.Reasons)
	}
}

func TestPatternScopedExceptionDoesNotWaivePlaceholder(t *testing.T) {
	in := Input{
		Method: "POST", Host: "cdn.example.com", Path: "/v1",
		Scan:                scanner.Result{Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: "piso_x_aaa"}}},
		SecretByPlaceholder: map[string]model.Secret{"piso_x_aaa": {ID: "s1", Placeholder: "piso_x_aaa"}},
		Exceptions: []model.Exception{{
			ID: "e-jwt", Enabled: true, HostRegex: `^cdn\.example\.com$`, PatternID: "jwt",
		}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonNoSecretRule {
		t.Fatalf("pattern exception must not skip substitution, got %q %+v", d.Action, d.Reasons)
	}
}

func TestExceptionAllowsPatternHit(t *testing.T) {
	in := Input{
		Method: "POST", Host: "sandbox.example.com", Path: "/echo",
		Body: `{"payload":"eyJtestonlyabc.def.ghi"}`,
		Scan: scanner.Result{PatternHits: []model.Finding{{Kind: model.FindingPatternSecret, PatternID: "jwt"}}},
		Exceptions: []model.Exception{{
			ID: "e1", Enabled: true, HostRegex: `sandbox\.example\.com`, PathRegex: `/echo`,
			ContentRegex: `eyJtestonly`,
		}},
	}
	d := Decide(in)
	if d.Action != model.ActionAllow || d.Reasons[0] != model.ReasonAllowedException {
		t.Fatalf("exception must allow, got %q %+v", d.Action, d.Reasons)
	}
}

func TestInternalTargetPolicy(t *testing.T) {
	// loopback is internal → policy blocks it. Only .piso.local control-plane
	// hosts and container-network names are exempt.
	in := Input{Host: "127.0.0.1", Path: "/", Scan: scanner.Result{}}
	d := Decide(in)
	if d.Action != model.ActionBlock {
		t.Fatalf("loopback must block, got %q %+v", d.Action, d.Reasons)
	}
}

func TestControlPlaneHostAllowed(t *testing.T) {
	in := Input{Host: "gateway.piso.local", Path: "/api/v1/secrets", Scan: scanner.Result{}}
	d := Decide(in)
	if d.Action != model.ActionAllow {
		t.Fatalf("control plane must be reachable from worker: %q %+v", d.Action, d.Reasons)
	}
}

func TestScopedSecretSubstitutesOnlyForAllowedWorker(t *testing.T) {
	sec := model.Secret{
		ID: "s_mon", Placeholder: "piso_mon_abc", Value: "sk-monitor-real",
		AllowedHosts: []string{"api.anthropic.com"}, Workers: []string{"monitor"},
	}
	base := Input{
		Method: "POST", Host: "api.anthropic.com", Path: "/v1/messages",
		Scan:                scanner.Result{Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: "piso_mon_abc"}}},
		SecretByPlaceholder: map[string]model.Secret{"piso_mon_abc": sec},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s_mon", Host: "api.anthropic.com", Placeholder: "piso_mon_abc"}},
	}
	// the designated monitor worker substitutes
	in := base
	in.Worker = "monitor"
	if d := Decide(in); d.Action != model.ActionSubstitute {
		t.Fatalf("monitor should substitute, got %q %+v", d.Action, d.Reasons)
	}
	// another worker cannot use the scoped key → no-secret-rule block
	in2 := base
	in2.Worker = "demo"
	d := Decide(in2)
	if d.Action != model.ActionBlock || len(d.Reasons) == 0 || d.Reasons[0] != model.ReasonNoSecretRule {
		t.Fatalf("other worker should be blocked, got %q %+v", d.Action, d.Reasons)
	}
}

func TestChatOnlyVaultPlaceholderDoesNotSubstitute(t *testing.T) {
	// A * rule must not rewrite piso_… sitting in messages[] — that is the
	// leak that put a real github_pat into the model prompt.
	ph := "piso_gh_abcdef"
	sec := model.Secret{ID: "s1", Placeholder: ph, Value: "github_pat_REALVALUE"}
	in := Input{
		Method: "POST", Host: "api.example.test", Path: "/v1/chat/completions",
		Scan: scanner.Result{Placeholders: []model.Finding{{
			Kind: model.FindingPlaceholder, Token: ph,
			Location: "json-body", Field: "messages[0].content",
		}}},
		SecretByPlaceholder: map[string]model.Secret{ph: sec},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "*", Placeholder: ph}},
	}
	d := Decide(in)
	if d.Action != model.ActionAllow {
		t.Fatalf("chat-only vault token must allow (not substitute), got %q %+v sub=%v", d.Action, d.Reasons, d.Substituted)
	}
	if len(d.Substituted) != 0 {
		t.Fatalf("chat-only token must not be on the substitute list: %v", d.Substituted)
	}
}

func TestChatOnlyVaultPlaceholderWithoutRuleDoesNotBlock(t *testing.T) {
	ph := "piso_gh_abcdef"
	in := Input{
		Method: "POST", Host: "api.example.test", Path: "/v1/chat/completions",
		Scan: scanner.Result{Placeholders: []model.Finding{{
			Kind: model.FindingPlaceholder, Token: ph,
			Location: "json-body", Field: "messages[3].tool_calls[0].function.arguments",
		}}},
		SecretByPlaceholder: map[string]model.Secret{ph: {ID: "s1", Placeholder: ph}},
	}
	d := Decide(in)
	if d.Action != model.ActionAllow {
		t.Fatalf("chat-only vault token without a rule must not no-secret-rule, got %q %+v", d.Action, d.Reasons)
	}
}

func TestHeaderAndChatSameTokenStillSubstitutes(t *testing.T) {
	ph := "piso_model_abc"
	sec := model.Secret{ID: "s1", Placeholder: ph, Value: "sk-real"}
	in := Input{
		Method: "POST", Host: "api.example.test", Path: "/v1/chat/completions",
		Scan: scanner.Result{Placeholders: []model.Finding{
			{Kind: model.FindingPlaceholder, Token: ph, Location: "authorization", Field: "Authorization"},
			{Kind: model.FindingPlaceholder, Token: ph, Location: "json-body", Field: "messages[0].content"},
		}},
		SecretByPlaceholder: map[string]model.Secret{ph: sec},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "api.example.test", Placeholder: ph}},
	}
	d := Decide(in)
	if d.Action != model.ActionSubstitute || len(d.Substituted) != 1 || d.Substituted[0] != ph {
		t.Fatalf("header occurrence must still substitute, got %q sub=%v %+v", d.Action, d.Substituted, d.Reasons)
	}
}

func TestNonChatJSONFieldStillNeedsRule(t *testing.T) {
	ph := "piso_gh_abcdef"
	in := Input{
		Method: "POST", Host: "api.example.com", Path: "/v1",
		Scan: scanner.Result{Placeholders: []model.Finding{{
			Kind: model.FindingPlaceholder, Token: ph,
			Location: "json-body", Field: "api_key",
		}}},
		SecretByPlaceholder: map[string]model.Secret{ph: {ID: "s1", Placeholder: ph}},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonNoSecretRule {
		t.Fatalf("non-chat body field without a rule must block, got %q %+v", d.Action, d.Reasons)
	}
}

func TestRealSecretInChatStillBlocks(t *testing.T) {
	in := Input{
		Method: "POST", Host: "api.example.test", Path: "/v1/chat/completions",
		Scan: scanner.Result{
			RealSecrets: []model.Finding{{
				Kind: model.FindingRealSecret, SecretID: "s1",
				Location: "json-body", Field: "messages[1].content",
			}},
			PatternHits: []model.Finding{{
				Kind: model.FindingPatternSecret, PatternID: "github-fine-grained",
				Location: "json-body", Field: "messages[1].content",
			}},
		},
	}
	d := Decide(in)
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonRealSecret {
		t.Fatalf("real secret in chat must still block, got %q %+v", d.Action, d.Reasons)
	}
}

func TestScopedSecretStarOrEmptyAllowsAll(t *testing.T) {
	for _, workers := range [][]string{nil, {"*"}} {
		sec := model.Secret{
			ID: "s", Placeholder: "piso_ph_abc", Value: "v",
			AllowedHosts: []string{"api.anthropic.com"}, Workers: workers,
		}
		in := Input{
			Method: "POST", Host: "api.anthropic.com", Path: "/v1/messages",
			Scan:                scanner.Result{Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: "piso_ph_abc"}}},
			SecretByPlaceholder: map[string]model.Secret{"piso_ph_abc": sec},
			Rules:               []model.Rule{{ID: "r", SecretID: "s", Host: "api.anthropic.com", Placeholder: "piso_ph_abc"}},
			Worker:              "demo",
		}
		if d := Decide(in); d.Action != model.ActionSubstitute {
			t.Fatalf("workers=%v should substitute for any worker, got %q %+v", workers, d.Action, d.Reasons)
		}
	}
}

func githubAppInput(ownerPath string, view model.GithubAppView) Input {
	const ph = "piso_github_app_testxx"
	sec := model.Secret{ID: "s1", Placeholder: ph, Kind: model.SecretKindGithubApp, Value: ""}
	return Input{
		Method: "GET", Host: "api.github.com", Path: ownerPath,
		Scan: scanner.Result{Placeholders: []model.Finding{{
			Kind: model.FindingPlaceholder, Token: ph, Location: "authorization",
		}}},
		SecretByPlaceholder:    map[string]model.Secret{ph: sec},
		Rules:                  []model.Rule{{ID: "r1", SecretID: "s1", Host: "api.github.com", Placeholder: ph}},
		GithubAppByPlaceholder: map[string]model.GithubAppView{ph: view},
	}
}

func TestGithubAppNeedsRegister(t *testing.T) {
	d := Decide(githubAppInput("/repos/complex-gh/piso", model.GithubAppView{}))
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonGithubAppRegisterRequired {
		t.Fatalf("got %q %+v", d.Action, d.Reasons)
	}
}

func TestGithubAppNeedsInstall(t *testing.T) {
	d := Decide(githubAppInput("/repos/complex-gh/piso", model.GithubAppView{HasPEM: true}))
	if d.Action != model.ActionBlock || d.Reasons[0] != model.ReasonGithubAppInstallRequired {
		t.Fatalf("got %q %+v", d.Action, d.Reasons)
	}
}

func TestGithubAppSubstitutesWhenInstalled(t *testing.T) {
	d := Decide(githubAppInput("/repos/complex-gh/piso", model.GithubAppView{HasPEM: true, Accounts: []string{"complex-gh"}}))
	if d.Action != model.ActionSubstitute {
		t.Fatalf("got %q %+v", d.Action, d.Reasons)
	}
}

// The allowlist must cover every provider's model-input field, not just
// OpenAI messages[]. A placeholder there is payload: allow, no rule needed.
func TestPayloadOnlyVaultPlaceholderAllows(t *testing.T) {
	ph := "piso_gh_abcdef"
	for _, field := range []string{
		"system",
		"input[0].content",
		"instructions",
		"contents[0].parts[0].text",
		"tools[0].function.description",
		"messages[0].api_key",
	} {
		t.Run(field, func(t *testing.T) {
			in := Input{
				Method: "POST", Host: "api.example.test", Path: "/v1/messages",
				Scan: scanner.Result{Placeholders: []model.Finding{{
					Kind: model.FindingPlaceholder, Token: ph,
					Location: "json-body", Field: field,
				}}},
				SecretByPlaceholder: map[string]model.Secret{ph: {ID: "s1", Placeholder: ph, Value: "real"}},
				Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "api.example.test", Placeholder: ph}},
			}
			d := Decide(in)
			if d.Action != model.ActionAllow || len(d.Substituted) != 0 {
				t.Fatalf("payload field %q: want allow/no-substitute, got %q sub=%v %+v", field, d.Action, d.Substituted, d.Reasons)
			}
			for _, r := range d.Reasons {
				if r == model.ReasonNoSecretRule {
					t.Fatalf("payload-only placeholder must not require a rule: %+v", d.Reasons)
				}
			}
		})
	}
}

// A credential-named leaf outside payload ancestry still substitutes.
func TestCredentialNamedLeafSubstitutes(t *testing.T) {
	ph := "piso_gh_abcdef"
	in := Input{
		Method: "POST", Host: "api.example.test", Path: "/v1",
		Scan: scanner.Result{Placeholders: []model.Finding{{
			Kind: model.FindingPlaceholder, Token: ph,
			Location: "json-body", Field: "config.api_key",
		}}},
		SecretByPlaceholder: map[string]model.Secret{ph: {ID: "s1", Placeholder: ph, Value: "real"}},
		Rules:               []model.Rule{{ID: "r1", SecretID: "s1", Host: "api.example.test", Placeholder: ph}},
	}
	d := Decide(in)
	if d.Action != model.ActionSubstitute || len(d.Substituted) != 1 {
		t.Fatalf("want substitute, got %q sub=%v %+v", d.Action, d.Substituted, d.Reasons)
	}
}
