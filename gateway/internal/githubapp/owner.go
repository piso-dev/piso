// Package githubapp mints GitHub App installation tokens and parses the
// repository owner from an egress request so one placeholder can map to
// many installations.
package githubapp

import (
	"encoding/json"
	"strings"
)

// AllowedHosts is the substitution host list for github-app secrets.
var AllowedHosts = []string{
	"github.com",
	"api.github.com",
	"uploads.github.com",
	"codeload.github.com",
}

// Owner extracts the GitHub account/org login from an HTTP request to
// github.com / api.github.com. Empty means "no owner in this request"
// (the minter may fall back to a single installation).
func Owner(host, path, body string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	path = strings.TrimPrefix(path, "/")
	if q := strings.Index(path, "?"); q >= 0 {
		path = path[:q]
	}
	switch host {
	case "uploads.github.com":
		return apiOwner(path)
	case "github.com", "www.github.com", "codeload.github.com":
		if o := firstPathOwner(path, githubSiteReserved); o != "" {
			return o
		}
	case "api.github.com":
		if o := apiOwner(path); o != "" {
			return o
		}
	}
	// Only GraphQL variables provide routing hints from a body. Arbitrary
	// REST payloads (issue text, PR head owners, etc.) are not route selectors.
	if host == "api.github.com" && path == "graphql" {
		return ownerFromJSON(body)
	}
	return ""
}

func apiOwner(path string) string {
	parts := splitPath(path)
	if len(parts) == 0 {
		return ""
	}
	switch parts[0] {
	case "repos", "repositories":
		if len(parts) >= 2 {
			return normalizeOwner(parts[1])
		}
	case "orgs", "users", "organizations":
		if len(parts) >= 2 {
			return normalizeOwner(parts[1])
		}
	case "graphql":
		return ""
	}
	return ""
}

func firstPathOwner(path string, reserved map[string]bool) string {
	parts := splitPath(path)
	if len(parts) == 0 {
		return ""
	}
	head := strings.ToLower(parts[0])
	if reserved[head] {
		return ""
	}
	return normalizeOwner(parts[0])
}

func splitPath(path string) []string {
	var out []string
	for _, p := range strings.Split(path, "/") {
		if p == "" || p == "." {
			continue
		}
		out = append(out, p)
	}
	return out
}

func normalizeOwner(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".git")
	return strings.ToLower(s)
}

func ownerFromJSON(body string) string {
	body = strings.TrimSpace(body)
	if body == "" || (body[0] != '{' && body[0] != '[') {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return ""
	}
	owners := map[string]bool{}
	collectOwners(v, owners, 0)
	if len(owners) > 1 {
		return "!ambiguous" // Never fall back to the single-installation route.
	}
	if len(owners) == 0 {
		return ""
	}
	for owner := range owners {
		return owner
	}
	return ""
}

func findOwner(v any, depth int) string {
	if depth > 6 {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		if o := stringField(t, "owner", "org", "organization", "login"); o != "" {
			// "login" alone is too broad (user objects). Only treat it as
			// owner when a sibling suggests a repo/org payload.
			if _, hasLogin := t["login"]; hasLogin {
				if _, ok := t["owner"]; !ok {
					if _, ok := t["full_name"]; !ok {
						if _, ok := t["name"]; !ok && t["type"] == nil {
							// keep looking
						}
					}
				}
			}
			if o = stringField(t, "owner", "org", "organization"); o != "" {
				return strings.ToLower(o)
			}
		}
		if nested, ok := t["owner"].(map[string]any); ok {
			if o := stringField(nested, "login", "name"); o != "" {
				return strings.ToLower(o)
			}
		}
		if nested, ok := t["repository"].(map[string]any); ok {
			if o := findOwner(nested, depth+1); o != "" {
				return o
			}
		}
		if nested, ok := t["organization"].(map[string]any); ok {
			if o := stringField(nested, "login", "name"); o != "" {
				return strings.ToLower(o)
			}
		}
		if vars, ok := t["variables"].(map[string]any); ok {
			if o := findOwner(vars, depth+1); o != "" {
				return o
			}
		}
		for _, key := range []string{"owner", "org", "login"} {
			if o, _ := t[key].(string); o != "" && key != "login" {
				return strings.ToLower(o)
			}
		}
		for _, child := range t {
			if o := findOwner(child, depth+1); o != "" {
				return o
			}
		}
	case []any:
		for _, child := range t {
			if o := findOwner(child, depth+1); o != "" {
				return o
			}
		}
	}
	return ""
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case map[string]any:
			if o := stringField(v, "login", "name"); o != "" {
				return o
			}
		}
	}
	return ""
}

// github.com first-path segments that are not account logins.
var githubSiteReserved = map[string]bool{
	"login": true, "logout": true, "session": true, "sessions": true,
	"settings": true, "notifications": true, "marketplace": true,
	"explore": true, "topics": true, "collections": true, "events": true,
	"sponsors": true, "pricing": true, "features": true, "enterprise": true,
	"security": true, "about": true, "team": true, "orgs": true,
	"organizations": true, "apps": true, "integrations": true, "new": true,
	"pulls": true, "issues": true, "search": true, "dashboard": true,
	"account": true, "auth": true, "oauth": true, "join": true,
	"stars": true, "watching": true, "codespaces": true, "copilot": true,
	"gist": true, "gitignore": true, "site": true, "readme": true,
}

// collectOwners gathers owner candidates instead of guessing a single one:
// a body with several distinct owners is ambiguous, not a route selector.
func collectOwners(v any, owners map[string]bool, depth int) {
	if depth > 6 {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		if nested, ok := t["owner"].(map[string]any); ok {
			if o := stringField(nested, "login", "name"); o != "" {
				owners[strings.ToLower(o)] = true
			}
		} else if o, _ := t["owner"].(string); o != "" {
			owners[strings.ToLower(o)] = true
		}
		if o, _ := t["org"].(string); o != "" {
			owners[strings.ToLower(o)] = true
		}
		if o := stringField(t, "organization"); o != "" {
			owners[strings.ToLower(o)] = true
		}
		for _, child := range t {
			collectOwners(child, owners, depth+1)
		}
	case []any:
		for _, child := range t {
			collectOwners(child, owners, depth+1)
		}
	}
}
