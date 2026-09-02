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
	Method  string
	Schema  string // "http" | "https"
	Host    string // hostname excluding port
	Path    string
	Body    string // raw body (for exception content matching)
	Scan    scanner.Result
	SecretByPlaceholder map[string]model.Secret // placeholder -> secret (enabled only)
	Rules   []model.Rule
	Domains map[string]model.DomainPolicy // keyed by lowercase host
	Exceptions []model.Exception
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

	// 3. Credential-looking content (real secrets + patterns).
	if in.Scan.LooksCredential() {
		if exceptionApplies(in, in.Scan) {
			appendReason(model.ReasonAllowedException)
			return model.Decision{Action: model.ActionAllow, Reasons: reasons}
		}
		for _, f := range in.Scan.RealSecrets {
			_ = f
			appendReason(model.ReasonRealSecret)
			break
		}
		if len(in.Scan.PatternHits) > 0 {
			appendReason(model.ReasonSecretPattern)
		}
		return model.Decision{Action: model.ActionBlock, Reasons: reasons}
	}

	// 4. Placeholders: substitute only with a matching rule; else block.
	if in.Scan.HasPlaceholder() {
		if exceptionApplies(in, in.Scan) {
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

	// 5. Plain request: allow by default.
	appendReason(model.ReasonAllowedDomain)
	return model.Decision{Action: model.ActionAllow, Reasons: reasons}
}

// substituteAll applies rules to every placeholder in the request. It returns
// the substituted placeholder list and whether ALL placeholders resolved.
func substituteAll(in Input) ([]string, bool) {
	seen := map[string]bool{}
	var out []string
	for _, f := range in.Scan.Placeholders {
		if seen[f.Token] {
			continue
		}
		seen[f.Token] = true
		if _, ok := lookupRule(in, f.Token); ok {
			out = append(out, f.Token)
		} else {
			return out, false // any unresolvable placeholder blocks the request
		}
	}
	return out, true
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

// exceptionApplies returns true when any enabled exception matches the
// request host/path and covers its credential findings.
func exceptionApplies(in Input, s scanner.Result) bool {
	for _, e := range in.Exceptions {
		if !e.Enabled {
			continue
		}
		if e.HostRegex != "" && !regexMatch(e.HostRegex, in.Host) {
			continue
		}
		if e.PathRegex != "" && !regexMatch(e.PathRegex, in.Path) {
			continue
		}
		if e.ContentRegex != "" {
			if !regexMatch(e.ContentRegex, in.Body) {
				continue
			}
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
				continue
			}
		}
		return true
	}
	return false
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