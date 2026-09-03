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
// internal-target block so the worker may reach the control API.
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
	leftover := nonPlaceholderPatternHits(in.Scan)
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
			return model.Decision{Action: model.ActionSubstitute, Reasons: reasons, Substituted: substituted}
		}
		appendReason(model.ReasonNoSecretRule)
		return model.Decision{Action: model.ActionBlock, Reasons: reasons}
	}

	// 6. Plain request: allow by default.
	appendReason(model.ReasonAllowedDomain)
	return model.Decision{Action: model.ActionAllow, Reasons: reasons}
}

// substituteAll applies rules to every vault placeholder in the request.
// Tokens that are not in the secret store (piso_vpc, piso_egress, chat
// mentioning piso_…) are not credentials and do not fail the request.
// It returns the substituted list and whether every vault placeholder resolved.
func substituteAll(in Input) ([]string, bool) {
	seen := map[string]bool{}
	var out []string
	for _, f := range in.Scan.Placeholders {
		if seen[f.Token] {
			continue
		}
		seen[f.Token] = true
		if !isVaultPlaceholder(in, f.Token) {
			continue
		}
		if _, ok := lookupRule(in, f.Token); ok {
			out = append(out, f.Token)
		} else {
			return out, false // any unresolvable vault placeholder blocks
		}
	}
	return out, true
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

// nonPlaceholderPatternHits drops pattern findings that are the piso_
// placeholder itself (Authorization: Bearer piso_…). Sample tokens are
// truncated, so a hit that shares location+field with a scanned placeholder
// is treated the same way.
func nonPlaceholderPatternHits(s scanner.Result) []model.Finding {
	var out []model.Finding
	for _, hit := range s.PatternHits {
		if patternHitIsPlaceholder(hit, s.Placeholders) {
			continue
		}
		out = append(out, hit)
	}
	return out
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
