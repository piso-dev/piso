// Package model defines the piso gateway data model: secrets, substitution
// rules, detection patterns, exceptions, routes, and the request log.
package model

import (
	"time"
)

// PlaceholderPrefix is the marker for fake credentials issued by piso.
// Anything matching piso_<...> inside a request is a placeholder: never a real
// secret, and only substituted when a rule allows it.
const PlaceholderPrefix = "piso_"

// Secret is a real credential plus its placeholder. Real values live ONLY in
// the gateway (memory + host-mounted file). The worker only ever sees the
// placeholder.
type Secret struct {
	ID           string    `json:"id"`                     // stable id
	Name         string    `json:"name"`                   // human name, e.g. "anthropic"
	Placeholder  string    `json:"placeholder"`            // e.g. "piso_anthropic_abc123"
	Value        string    `json:"value"`                  // real credential. never logged.
	AllowedHosts []string  `json:"allowedHosts,omitempty"` // hosts where substitution is allowed
	Workers      []string  `json:"workers,omitempty"`      // empty or "*" = all workers
	CreatedAt    time.Time `json:"createdAt"`
}

// Rule is a substitution rule: substitute placeholder P for real value on
// host H. Rules are the ONLY way a placeholder turns into a real credential.
type Rule struct {
	ID          string `json:"id"`
	SecretID    string `json:"secretId"`    // which secret this resolves to
	Host        string `json:"host"`        // host this rule applies to (may be "*")
	Placeholder string `json:"placeholder"` // piso_ token; empty = any of the secret's
	Note        string `json:"note,omitempty"`
}

// Pattern is one entry of the "looks like a credential" regex library.
// Regexes are compiled at load; the library is user-updatable.
type Pattern struct {
	ID          string `json:"id"`
	Name        string `json:"name"`  // e.g. "openai-sk", "aws-access-key"
	Regex       string `json:"regex"` // RE2 syntax
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Severity    string `json:"severity"` // "high" | "medium" | "low"
}

// Exception is a user-configured override. If a request matches all present
// fields of an exception, credential-style findings are allowed (blocked
// requests become allowed). Exceptions NEVER substitute placeholders; they
// only stop the block.
//
// PatternID, when set, scopes the exception to that credential pattern
// (e.g. "jwt"). Pattern-scoped exceptions never waive real-secret-detected
// or no-secret-rule; a request is allowed only when every pattern hit is
// covered. An exception with empty PatternID is host-wide (legacy behavior).
type Exception struct {
	ID           string `json:"id"`
	Note         string `json:"note,omitempty"`
	HostRegex    string `json:"hostRegex,omitempty"`    // optional; match against request host
	PathRegex    string `json:"pathRegex,omitempty"`    // optional; match against request path
	ContentRegex string `json:"contentRegex,omitempty"` // optional; match against request body
	Placeholder  string `json:"placeholder,omitempty"`  // optional; exempt this specific piso_ token
	PatternID    string `json:"patternId,omitempty"`    // optional; cover only this pattern
	Enabled      bool   `json:"enabled"`
}

// DomainPolicy is the per-host policy that isn't "default".
// Default posture: allow-by-default; credentialed requests need a rule.
type DomainPolicy struct {
	Host      string `json:"host"`
	Deny      bool   `json:"deny"`      // hard block, regardless of credentials
	NeedsRule bool   `json:"needsRule"` // credentialed requests must have a substitution rule
	Note      string `json:"note,omitempty"`
}

// Route is an ingress mapping: name.piso.local -> worker:port.
type Route struct {
	ID     string `json:"id"`
	Name   string `json:"name"`   // subdomain label, e.g. "preview"
	Worker string `json:"worker"` // worker container name/host, e.g. "worker-myproj"
	Port   int    `json:"port"`   // port on the worker
	Note   string `json:"note,omitempty"`
}

// FindingKind classifies what the scanner found in a request.
type FindingKind string

const (
	FindingPlaceholder   FindingKind = "placeholder" // a piso_ token
	FindingRealSecret    FindingKind = "real-secret" // exact match against a stored secret value
	FindingPatternSecret FindingKind = "pattern"     // matched a credential-looking regex
)

// Finding is one detection within a request.
type Finding struct {
	Kind      FindingKind `json:"kind"`
	PatternID string      `json:"patternId,omitempty"` // for pattern findings
	SecretID  string      `json:"secretId,omitempty"`  // for real-secret findings
	Token     string      `json:"token,omitempty"`     // the matched string (placeholder or truncated secret)
	Location  string      `json:"location"`            // "authorization" | "x-api-key" | "cookie" | "query" | "json-body" | "body" | "header:<name>"
	Field     string      `json:"field,omitempty"`     // JSON field / query param name
}

// Decision is the gateway's verdict for a scanned request.
type Decision struct {
	Action      Action   `json:"action"`
	Reasons     []Reason `json:"reasons"`
	Substituted []string `json:"substituted,omitempty"` // placeholders that were replaced
}

// Action is what happens to a request.
type Action string

const (
	ActionAllow      Action = "allow"
	ActionSubstitute Action = "substitute"
	ActionBlock      Action = "block"
)

// Reason codes are machine-readable and stable — the UI renders them, the
// worker gets only the code + request id (never the detail).
type Reason string

const (
	ReasonNoSecretRule     Reason = "no-secret-rule"       // placeholder present, no substitution rule
	ReasonRealSecret       Reason = "real-secret-detected" // exact known credential found
	ReasonSecretPattern    Reason = "credential-pattern"   // looks like a credential (regex hit)
	ReasonDeniedDomain     Reason = "denied-domain"        // host on deny list
	ReasonInternalTarget   Reason = "internal-target"      // internal/link-local target blocked
	ReasonAllowedException Reason = "allowed-exception"    // user exception; informational, not a block
	ReasonAllowedDomain    Reason = "allowed-domain"       // informational
)

// RequestRecord is the request log entry. Retryable requests carry the full
// captured request so the gateway can replay them after a rule is added.
// Real secret values are NEVER stored here — only placeholder names.
type RequestRecord struct {
	ID        string    `json:"id"`
	Worker    string    `json:"worker"`
	Ts        time.Time `json:"ts"`
	Method    string    `json:"method"`
	Scheme    string    `json:"scheme"`
	Host      string    `json:"host"`
	Path      string    `json:"path"`
	Status    int       `json:"status"` // 200 allowed/substituted, 407 blocked
	Action    Action    `json:"action"`
	Reasons   []Reason  `json:"reasons"`
	Findings  []Finding `json:"findings"`
	RequestID string    `json:"requestId"` // correlation id for worker
	Retryable bool      `json:"retryable"`
	// Captured request for replay (body is a placeholder-bearing snapshot).
	Capture *CapturedRequest `json:"capture,omitempty"`
}

// CapturedRequest is the snapshot needed to replay a blocked request.
type CapturedRequest struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body"`
}

// SecretSummary is the log-safe view of a secret (never Value).
type SecretSummary struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Placeholder  string    `json:"placeholder"`
	EnvKey       string    `json:"envKey,omitempty"`
	AllowedHosts []string  `json:"allowedHosts,omitempty"`
	Workers      []string  `json:"workers,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

// ToSummary strips the real value.
func (s *Secret) ToSummary() SecretSummary {
	return SecretSummary{
		ID:           s.ID,
		Name:         s.Name,
		Placeholder:  s.Placeholder,
		AllowedHosts: s.AllowedHosts,
		Workers:      s.Workers,
		CreatedAt:    s.CreatedAt,
	}
}
