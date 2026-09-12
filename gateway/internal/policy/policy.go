// Package policy turns scanner findings into a Decision. It is the gateway's
// enforcement point: placeholders substitute only when a rule exists; anything
// that looks like a real credential blocks unless a user exception applies;
// denied domains and internal targets hard-block.
package policy

import (
	"net"
	"regexp"
	"strings"
	"sync"

	"piso/gateway/internal/model"
	"piso/gateway/internal/scanner"
)

// DNS name of the gateway's own ingress/control plane, exempted from the
// internal-target block so the worker may reach the worker API on the vpc.
const (
	ControlHost    = "gateway.piso.local"
	InternalSuffix = ".piso.local"
)

// Input is everything the policy needs about a request + configured state.
type Input struct {
	Method              string
	Schema              string // "http" | "https"
	Host                string // hostname excluding port
	Path                string
	Body                string // raw body (for exception content matching)
	Scan                scanner.Result
	SecretByPlaceholder map[string]model.Secret // placeholder -> secret (enabled only)
	Rules               []model.Rule
	Domains             map[string]model.DomainPolicy // keyed by lowercase host
	Exceptions          []model.Exception
	// Worker is the origin worker SLUG (e.g. "demo", "monitor"). Used to
	// enforce Secret.Workers scoping: a secret whose Workers list is non-empty
	// only substitutes for a listed worker. Empty Workers = all workers.
	Worker string
}

// Decide runs the pipeline and returns the action plus reasons.
func Decide(in Input) model.Decision {
	var reasons []model.Reason
	appendReason := func(r model.Reason) { reasons = append(reasons, r) }

	// 1. Internal / control-plane targets: loopback, link-local, private
	//    ranges and metadata are hard-blocked. .piso.local and container
	//    service names are the gateway's own and are allowed (the worker
	//    legitimately calls the control API — which is on the internal
	//    network and never passes through the egress proxy anyway).
	ip := net.ParseIP(in.Host)
	if isInternal(in.Host, ip) {
		appendReason(model.ReasonInternalTarget)
		return model.Decision{Action: model.ActionBlock, Reasons: reasons}
	}

	// 2. Denied domains hard-block regardless of credentials.
	if dp, ok := in.Domains[strings.ToLower(in.Host)]; ok && dp.Deny {
		appendReason(model.ReasonDeniedDomain)
		return model.Decision{Action: model.ActionBlock, Reasons: reasons}
	}

	// 3. Exact known real secrets always block (unless a host-wide exception).
	// Pattern-scoped exceptions never waive an exact real secret.
	if len(in.Scan.RealSecrets) > 0 {
		if hostWideExceptionApplies(in, in.Scan) {
			appendReason(model.ReasonAllowedException)
			return model.Decision{Action: model.ActionAllow, Reasons: reasons}
		}
		appendReason(model.ReasonRealSecret)
		if len(in.Scan.PatternHits) > 0 {
			appendReason(model.ReasonSecretPattern)
		}
		return model.Decision{Action: model.ActionBlock, Reasons: reasons}
	}

	// 4. Credential-looking patterns. Hits that are just a piso_ placeholder
	// (e.g. Authorization: Bearer piso_routstr_…) are not secrets — they fall
	// through to substitution. A leftover real-looking hit still blocks
	// unless every such hit is covered by an exception.
	leftover := egressPatternHits(in.Scan)
	if len(leftover) > 0 {
		if allHitsCovered(in, leftover) {
			if !in.Scan.HasPlaceholder() {
				appendReason(model.ReasonAllowedException)
				return model.Decision{Action: model.ActionAllow, Reasons: reasons}
			}
			// Excepted patterns plus placeholders: keep going so the
			// placeholder still substitutes.
		} else {
			appendReason(model.ReasonSecretPattern)
			return model.Decision{Action: model.ActionBlock, Reasons: reasons}
		}
	}

	// 5. Placeholders: substitute only with a matching rule; else block.
	// Pattern-scoped exceptions do not skip substitution.
	if in.Scan.HasPlaceholder() {
		if hostWideExceptionApplies(in, in.Scan) {
			appendReason(model.ReasonAllowedException)
			return model.Decision{Action: model.ActionAllow, Reasons: reasons}
		}
		substituted, ok := substituteAll(in)
		if ok {
			appendReason(model.ReasonAllowedDomain)
			if len(substituted) == 0 {
				// Chat-only / non-vault piso_… tokens: nothing to rewrite.
				return model.Decision{Action: model.ActionAllow, Reasons: reasons}
			}
			return model.Decision{Action: model.ActionSubstitute, Reasons: reasons, Substituted: substituted}
		}
		appendReason(model.ReasonNoSecretRule)
		return model.Decision{Action: model.ActionBlock, Reasons: reasons}
	}

	// 6. Plain request: allow by default.
	appendReason(model.ReasonAllowedDomain)
	return model.Decision{Action: model.ActionAllow, Reasons: reasons}
}

// substituteAll applies rules to every vault placeholder in a credential
// location (headers, query, non-messages JSON). Tokens that are not in the
// secret store (piso_vpc, piso_egress) are not credentials and do not fail
// the request. Vault placeholders that appear *only* in the LLM transcript
// (messages[]) are left as text — substituting them leaks the real secret
// to the model. They do not require a host rule and do not no-secret-rule.
// It returns the substituted list and whether every credential-site vault
// placeholder resolved.
func substituteAll(in Input) ([]string, bool) {
	seen := map[string]bool{}
	nonChat := map[string]bool{}
	var tokens []string
	for _, f := range in.Scan.Placeholders {
		if !isVaultPlaceholder(in, f.Token) {
			continue
		}
		if !seen[f.Token] {
			seen[f.Token] = true
			tokens = append(tokens, f.Token)
		}
		if !scanner.IsChatContent(f.Location, f.Field) {
			nonChat[f.Token] = true
		}
	}
	var out []string
	for _, tok := range tokens {
		if !nonChat[tok] {
			continue // transcript-only: do not substitute, do not block
		}
		if _, ok := lookupRule(in, tok); ok && in.secretAllowedForWorker(tok) {
			out = append(out, tok)
		} else {
			return out, false // any unresolvable credential-site vault placeholder blocks
		}
	}
	return out, true
}

// secretAllowedForWorker reports whether the origin worker may substitute this
// placeholder's secret. Secret.Workers scoping: non-empty list = ONLY those
// slugs may use it ("*" = all); empty = all. A worker outside the list is
// treated as having no rule → blocked no-secret-rule (retryable).
func (in Input) secretAllowedForWorker(token string) bool {
	sec, ok := in.SecretByPlaceholder[token]
	if !ok || len(sec.Workers) == 0 {
		return true
	}
	for _, w := range sec.Workers {
		if w == "*" || w == in.Worker {
			return true
		}
	}
	return false
}

// isVaultPlaceholder reports whether token is an issued secret placeholder.
func isVaultPlaceholder(in Input, token string) bool {
	_, ok := in.SecretByPlaceholder[token]
	return ok
}

// lookupRule finds a substitution rule for placeholder on this host.
// A rule matches when: same placeholder, and (host == "*" or host == this host).
func lookupRule(in Input, placeholder string) (model.Rule, bool) {
	host := strings.ToLower(in.Host)
	for _, r := range in.Rules {
		if r.Placeholder != placeholder {
			continue
		}
		if r.Host == "*" || strings.EqualFold(r.Host, host) {
			return r, true
		}
	}
	return model.Rule{}, false
}

// hostWideExceptionApplies reports whether any enabled exception without a
// PatternID matches the request. Used to waive real secrets and placeholders.
func hostWideExceptionApplies(in Input, s scanner.Result) bool {
	for _, e := range in.Exceptions {
		if e.PatternID != "" {
			continue
		}
		if exceptionRequestMatch(e, in, s) {
			return true
		}
	}
	return false
}

// egressPatternHits are pattern findings that are actually leaving as
// credentials: not a wrapped piso_ token, and not chat/message text.
// A JWT or sk- example inside messages[n].content is conversation, not
// an Authorization header.
func egressPatternHits(s scanner.Result) []model.Finding {
	var out []model.Finding
	for _, hit := range s.PatternHits {
		if patternHitIsPlaceholder(hit, s.Placeholders) {
			continue
		}
		if patternHitIsChatContent(hit) {
			continue
		}
		out = append(out, hit)
	}
	return out
}

// patternHitIsChatContent reports whether the hit is inside the LLM
// messages[] transcript, not a request-level credential field.
func patternHitIsChatContent(hit model.Finding) bool {
	return scanner.IsChatContent(hit.Location, hit.Field)
}

// patternHitIsPlaceholder reports whether a pattern finding is just a
// wrapped piso_ token, not a separate leaked credential.
func patternHitIsPlaceholder(hit model.Finding, phs []model.Finding) bool {
	for _, p := range phs {
		if p.Token != "" && strings.Contains(hit.Token, p.Token) {
			return true
		}
		if hit.Location != "" && hit.Location == p.Location && strings.EqualFold(hit.Field, p.Field) {
			return true
		}
	}
	return false
}

// allHitsCovered reports whether every given pattern finding is covered by
// some matching exception (host-wide or that PatternID).
func allHitsCovered(in Input, hits []model.Finding) bool {
	if len(hits) == 0 {
		return false
	}
	for _, hit := range hits {
		if !patternHitCovered(in, in.Scan, hit.PatternID) {
			return false
		}
	}
	return true
}

// patternHitCovered reports whether any enabled exception covers patternID.
func patternHitCovered(in Input, s scanner.Result, patternID string) bool {
	for _, e := range in.Exceptions {
		if e.PatternID != "" && e.PatternID != patternID {
			continue
		}
		if exceptionRequestMatch(e, in, s) {
			return true
		}
	}
	return false
}

// exceptionRequestMatch checks Enabled plus host/path/content/placeholder.
func exceptionRequestMatch(e model.Exception, in Input, s scanner.Result) bool {
	if !e.Enabled {
		return false
	}
	if e.HostRegex != "" && !regexMatch(e.HostRegex, in.Host) {
		return false
	}
	if e.PathRegex != "" && !regexMatch(e.PathRegex, in.Path) {
		return false
	}
	if e.ContentRegex != "" && !regexMatch(e.ContentRegex, in.Body) {
		return false
	}
	if e.Placeholder != "" {
		has := false
		for _, f := range s.Placeholders {
			if f.Token == e.Placeholder {
				has = true
				break
			}
		}
		if !has {
			return false
		}
	}
	return true
}

// regexMatch applies a RE2 pattern via a small sync.Map cache (exception
// checks are off the hot path, but pattern sets can be edited live).
var regexCache sync.Map // string -> *regexp.Regexp

func regexMatch(pattern, s string) bool {
	if v, ok := regexCache.Load(pattern); ok {
		return v.(*regexp.Regexp).MatchString(s)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	regexCache.Store(pattern, re)
	return re.MatchString(s)
}

// isInternal reports whether host is an internal/link-local/metadata target.
func isInternal(host string, ip net.IP) bool {
	if ip == nil {
		// hostname: exempt control-plane + container-network DNS names.
		if strings.HasSuffix(host, InternalSuffix) {
			return false // .piso.local hosts are the gateway's own, allowed
		}
		// localhost-style names
		h := strings.ToLower(host)
		if h == "localhost" || h == "gateway" || h == "worker" || strings.HasPrefix(h, "worker-") {
			return false
		}
		// Unknown hostnames resolve through the gateway's DNS; treat as external.
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() || ip.IsUnspecified() {
		return true
	}
	return false
}
