// Package gitaccess treats Git credentials as opaque tokens. Capability hints
// describe operator intent; only the upstream server authorizes operations.
package gitaccess

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const Kind = "git-token"

var Capabilities = []string{"read", "createBranch", "updateBranch", "deleteRef", "rewriteHistory", "openPR", "comment", "approve", "merge", "settings"}

type Config struct {
	Provider  string            `json:"provider"`
	TokenType string            `json:"tokenType"`
	BaseURL   string            `json:"baseURL"`
	Hints     map[string]string `json:"hints"`
}

// Normalize never contacts the host. Confirming the destination precedes any
// authenticated probe. HTTPS and exact origins are required, including Gitea.
func (c Config) Normalize() (Config, []string, error) {
	if c.Provider != "github" && c.Provider != "gitea" {
		return c, nil, fmt.Errorf("provider must be github or gitea")
	}
	if c.TokenType == "" {
		c.TokenType = "user"
	}
	if c.TokenType != "user" && !(c.Provider == "github" && c.TokenType == "installation") {
		return c, nil, fmt.Errorf("unsupported credential type")
	}
	if c.BaseURL == "" && c.Provider == "github" {
		c.BaseURL = "https://github.com"
	}
	u, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Path, "..") || strings.Contains(u.Host, "*") {
		return c, nil, fmt.Errorf("enter an HTTPS instance URL without credentials, query, fragment, or wildcards")
	}
	c.BaseURL = strings.TrimRight(u.String(), "/")
	if c.Provider == "github" && u.Path != "" && u.Path != "/" {
		return c, nil, fmt.Errorf("GitHub instance URL must be an origin")
	}
	hints := map[string]string{}
	for key, value := range c.Hints {
		known := false
		for _, name := range Capabilities {
			if key == name {
				known = true
				break
			}
		}
		if !known || (value != "allowed" && value != "denied" && value != "unknown") {
			return c, nil, fmt.Errorf("invalid capability hint")
		}
		hints[key] = value
	}
	c.Hints = hints
	hosts := []string{strings.ToLower(u.Hostname())}
	if c.Provider == "github" && u.Host == "github.com" {
		hosts = append(hosts, "api.github.com")
	}
	return c, hosts, nil
}

func (c Config) APIBase() string {
	if c.Provider == "github" {
		if c.BaseURL == "https://github.com" {
			return "https://api.github.com"
		}
		return c.BaseURL + "/api/v3"
	}
	return c.BaseURL + "/api/v1"
}

// Bundle lives inside the existing sealed secret envelope, not in public
// coordinator metadata. The vault still holds just the token for substitution.
type Bundle struct {
	Version int      `json:"version"`
	Config  Config   `json:"config"`
	Token   string   `json:"token"`
	Workers []string `json:"workers,omitempty"`
}

func Encode(c Config, token string, workers []string) (string, error) {
	b, err := json.Marshal(Bundle{Version: 1, Config: c, Token: token, Workers: workers})
	return string(b), err
}
func Decode(plain string) (Bundle, error) {
	var b Bundle
	if err := json.Unmarshal([]byte(plain), &b); err != nil {
		return b, fmt.Errorf("invalid Git token bundle")
	}
	if b.Version != 1 || b.Token == "" {
		return b, fmt.Errorf("invalid Git token bundle")
	}
	c, _, err := b.Config.Normalize()
	b.Config = c
	return b, err
}
