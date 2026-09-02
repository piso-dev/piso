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
		Scan: scanner.Result{Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: "piso_anthropic_abc"}}},
		SecretByPlaceholder: map[string]model.Secret{"piso_anthropic_abc": sec},
		Rules: []model.Rule{{ID: "r1", SecretID: "s1", Host: "api.anthropic.com", Placeholder: "piso_anthropic_abc"}},
	}
	d := Decide(in)
	if d.Action != model.ActionSubstitute {
		t.Fatalf("want substitute, got %q %+v", d.Action, d.Reasons)
	}
	if len(d.Substituted) != 1 || d.Substituted[0] != "piso_anthropic_abc" {
		t.Fatalf("wrong substituted list: %+v", d.Substituted)
	}
}

func TestPlaceholderWithoutRuleBlocks(t *testing.T) {
	in := Input{
		Method: "POST", Host: "api.anthropic.com", Path: "/v1/messages",
		Scan: scanner.Result{Placeholders: []model.Finding{{Kind: model.FindingPlaceholder, Token: "piso_anthropic_abc"}}},
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