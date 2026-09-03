// Package patterns is the updatable "looks like a credential" regex library.
// Defaults ship embedded; users can override/extend via a JSON file mounted
// into the gateway. The scanner compiles all enabled patterns once at load.
package patterns

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"sync"
)

// DefaultLibrary is the seed set of credential-looking patterns. It is
// intentionally broad: the worker should never send real credentials, so a
// false positive just needs an exception entry. A missed real credential is
// the one thing we can't afford.
var DefaultLibrary = []Entry{
	{ID: "openai-sk", Name: "openai-sk", Regex: `\bsk-[A-Za-z0-9_-]{20,}\b`, Description: "OpenAI API key (sk-...)", Severity: "high", Enabled: true},
	{ID: "openai-sk-proj", Name: "openai-sk-proj", Regex: `\bsk-proj-[A-Za-z0-9_-]{20,}\b`, Description: "OpenAI project key", Severity: "high", Enabled: true},
	{ID: "anthropic-sk", Name: "anthropic-sk", Regex: `\bsk-ant-[A-Za-z0-9_-]{20,}\b`, Description: "Anthropic API key", Severity: "high", Enabled: true},
	{ID: "anthropic-sk-ant-03", Name: "anthropic-sk-ant-03", Regex: `\bsk-ant-api03-[A-Za-z0-9_-]{20,}\b`, Description: "Anthropic API key (api03 format)", Severity: "high", Enabled: true},
	{ID: "aws-access-key", Name: "aws-access-key", Regex: `\b(?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA)[0-9A-Z]{16}\b`, Description: "AWS access key id", Severity: "high", Enabled: true},
	{ID: "aws-secret", Name: "aws-secret", Regex: `(?i)\baws_secret_access_key\b\s*[=:]\s*['"]?[A-Za-z0-9/+=]{40}['"]?`, Description: "AWS secret access key assignment", Severity: "high", Enabled: true},
	{ID: "github-pat", Name: "github-pat", Regex: `\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b`, Description: "GitHub personal/user/app/refresh token", Severity: "high", Enabled: true},
	{ID: "github-fine-grained", Name: "github-fine-grained", Regex: `\bgithub_pat_[A-Za-z0-9_]{70,}\b`, Description: "GitHub fine-grained PAT", Severity: "high", Enabled: true},
	{ID: "slack-token", Name: "slack-token", Regex: `\bxox[baprs]-[0-9A-Za-z-]{10,}\b`, Description: "Slack bot/user/app token", Severity: "high", Enabled: true},
	{ID: "stripe-live", Name: "stripe-live", Regex: `\b(?:sk|rk)_live_[0-9a-zA-Z]{16,}\b`, Description: "Stripe live secret/restricted key", Severity: "high", Enabled: true},
	{ID: "stripe-test", Name: "stripe-test", Regex: `\b(?:sk|rk)_test_[0-9a-zA-Z]{16,}\b`, Description: "Stripe test key", Severity: "medium", Enabled: true},
	{ID: "google-api", Name: "google-api", Regex: `\bAIza[0-9A-Za-z\-_]{35}\b`, Description: "Google API key", Severity: "high", Enabled: true},
	{ID: "google-oauth", Name: "google-oauth", Regex: `\b[0-9]+-[0-9A-Za-z_]{32}\.apps\.googleusercontent\.com\b`, Description: "Google OAuth client id", Severity: "low", Enabled: true},
	{ID: "sendgrid", Name: "sendgrid", Regex: `\bSG\.[A-Za-z0-9_\-\.]{22,}\b`, Description: "SendGrid API key", Severity: "high", Enabled: true},
	{ID: "twilio", Name: "twilio", Regex: `\bSK[0-9a-fA-F]{32}\b`, Description: "Twilio API key", Severity: "high", Enabled: true},
	{ID: "npm-token", Name: "npm-token", Regex: `\bnpm_[A-Za-z0-9]{36}\b`, Description: "npm access token", Severity: "high", Enabled: true},
	{ID: "pypi-token", Name: "pypi-token", Regex: `\bpypi-AgEIcHlwaS5vcmc[A-Za-z0-9\-_]{50,}\b`, Description: "PyPI upload token", Severity: "high", Enabled: true},
	{ID: "private-key", Name: "private-key", Regex: `-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY(?: BLOCK)?-----`, Description: "Private key block", Severity: "high", Enabled: true},
	{ID: "jwt", Name: "jwt", Regex: `\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`, Description: "JWT (three dot-separated segments)", Severity: "medium", Enabled: true},
	{ID: "generic-bearer", Name: "generic-bearer", Regex: `(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{24,}\b`, Description: "Bearer token (long, non-placeholder)", Severity: "medium", Enabled: true},
	{ID: "basic-auth", Name: "basic-auth", Regex: `(?i)\bbasic\s+[A-Za-z0-9+/]{16,}={0,2}\b`, Description: "Basic auth payload (long b64)", Severity: "medium", Enabled: true},
	{ID: "password-assignment", Name: "password-assignment", Regex: `(?i)\b(password|passwd|pwd|secret)\s*[:=]\s*['"][^'"]{8,}['"]`, Description: "password=... assignment", Severity: "low", Enabled: true},
	{ID: "datadog", Name: "datadog", Regex: `\b[0-9a-f]{32}[A-Za-z0-9]{8}\b`, Description: "Datadog API key (40 hex-ish)", Severity: "low", Enabled: false},
	{ID: "discord-bot", Name: "discord-bot", Regex: `\b[MN][A-Za-z\d]{23}\.[\w-]{6}\.[\w-]{27,}\b`, Description: "Discord bot token", Severity: "high", Enabled: true},
	{ID: "ngrok", Name: "ngrok", Regex: `\b[0-9a-zA-Z]{22,26}\b`, Description: "ngrok agent token", Severity: "medium", Enabled: false},
	{ID: "gitlab-pat", Name: "gitlab-pat", Regex: `\bglpat-[A-Za-z0-9_-]{20,}\b`, Description: "GitLab personal access token", Severity: "high", Enabled: true},
	{ID: "bitbucket", Name: "bitbucket", Regex: `\bATBB[A-Za-z0-9]{24}\b`, Description: "Bitbucket app password", Severity: "high", Enabled: true},
	{ID: "digitalocean", Name: "digitalocean", Regex: `\bdop_v1_[a-f0-9]{64}\b`, Description: "DigitalOcean PAT", Severity: "high", Enabled: true},
	{ID: "pulumi", Name: "pulumi", Regex: `\bpul-[a-f0-9]{40}\b`, Description: "Pulumi access token", Severity: "high", Enabled: true},
	{ID: "terraform-cloud", Name: "terraform-cloud", Regex: `\batlas\.v1\.[A-Za-z0-9_-]{60,}\b`, Description: "Terraform Cloud token", Severity: "high", Enabled: true},
	{ID: "vault-token", Name: "vault-token", Regex: `\bhvs\.[A-Za-z0-9_-]{20,}\b`, Description: "HashiCorp Vault service token", Severity: "high", Enabled: true},
	{ID: "auth0", Name: "auth0", Regex: `\beyJhbGciOi[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\b`, Description: "Auth0 JWT", Severity: "medium", Enabled: false},
	{ID: "django-secret", Name: "django-secret", Regex: `(?i)django[_-]secret[_-]key\b\s*[=:]\s*['"][^'"]{20,}['"]`, Description: "Django SECRET_KEY assignment", Severity: "high", Enabled: true},
	{ID: "hex-64", Name: "hex-64", Regex: `\b[0-9a-fA-F]{64}\b`, Description: "64-char hex (token-like)", Severity: "low", Enabled: false},
	{ID: "base64-40", Name: "base64-40", Regex: `\b[A-Za-z0-9+/]{40,}={0,2}\b`, Description: "long base64 blob (token-like)", Severity: "low", Enabled: false},
}

// Entry is one pattern (JSON-friendly; DefaultLibrary entries use this shape).
type Entry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Regex       string `json:"regex"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
	Enabled     bool   `json:"enabled"`
}

// Compiled is a loaded, validated library.
type Compiled struct {
	mu       sync.RWMutex
	patterns map[string]CompiledPattern
	byID     []string // stable order
}

type CompiledPattern struct {
	Entry
	re *regexp.Regexp
}

// Load returns a Compiled library: defaults merged with the optional JSON
// file. File entries override defaults by ID; entries with empty ID append.
func Load(path string) (*Compiled, error) {
	c := &Compiled{patterns: map[string]CompiledPattern{}}
	for _, e := range DefaultLibrary {
		if err := c.add(e); err != nil {
			return nil, fmt.Errorf("default pattern %s: %w", e.ID, err)
		}
	}
	if path == "" {
		return c, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("patterns file %s: %w", path, err)
	}
	for _, e := range entries {
		if err := c.add(e); err != nil {
			return nil, fmt.Errorf("pattern %s: %w", e.ID, err)
		}
	}
	return c, nil
}

func (c *Compiled) add(e Entry) error {
	re, err := regexp.Compile(e.Regex)
	if err != nil {
		return fmt.Errorf("regex %q: %w", e.Regex, err)
	}
	c.patterns[e.ID] = CompiledPattern{Entry: e, re: re}
	// rebuild stable order
	c.byID = c.byID[:0]
	for id := range c.patterns {
		c.byID = append(c.byID, id)
	}
	sort.Strings(c.byID)
	return nil
}

// Match scans data with all enabled patterns, returning matcher-fulfilled
// matches in library order. The returned match strings are capped so log
// entries never store full credential material.
func (c *Compiled) Match(data []byte) []Match {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Match
	for _, id := range c.byID {
		p := c.patterns[id]
		if !p.Enabled {
			continue
		}
		loc := p.re.FindIndex(data)
		if loc == nil {
			continue
		}
		out = append(out, Match{
			PatternID: p.ID,
			Name:      p.Name,
			Severity:  p.Severity,
			Sample:    capSample(string(data[loc[0]:loc[1]])),
			Start:     loc[0],
			End:       loc[1],
		})
	}
	return out
}

// Entries returns the full library (including disabled entries) for the UI.
func (c *Compiled) Entries() []Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Entry, 0, len(c.byID))
	for _, id := range c.byID {
		out = append(out, c.patterns[id].Entry)
	}
	return out
}

// Match is one regex hit.
type Match struct {
	PatternID string `json:"patternId"`
	Name      string `json:"name"`
	Severity  string `json:"severity"`
	Sample    string `json:"sample"` // truncated — never full credential
	Start     int    `json:"-"`     // byte offset in the scanned buffer
	End       int    `json:"-"`     // exclusive byte offset
}

// capSample truncates a matched sample so it can't be a full credential leak
// in the log.
func capSample(s string) string {
	const max = 12
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}