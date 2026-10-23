package model

import "testing"

func TestHostRuleMatch(t *testing.T) {
	cases := []struct {
		rule, host string
		want       bool
	}{
		// exact
		{"api.anthropic.com", "api.anthropic.com", true},
		{"api.anthropic.com", "API.Anthropic.com", true},
		{"api.anthropic.com", "anthropic.com", false},
		{"api.anthropic.com", "x.api.anthropic.com", false},

		// subdomain wildcard
		{"*.tinfoil.sh", "router-0.tinfoil.sh", true},
		{"*.tinfoil.sh", "router-7.tinfoil.sh", true},
		{"*.tinfoil.sh", "inference.tinfoil.sh", true},
		{"*.tinfoil.sh", "Router-0.Tinfoil.sh", true},
		{"*.tinfoil.sh", "a.b.tinfoil.sh", true}, // multi-label is fine
		{"*.tinfoil.sh", "tinfoil.sh", false},    // apex never covered
		{"*.tinfoil.sh", "eviltinfoil.sh", false},
		{"*.tinfoil.sh", "tinfoil.sh.evil.com", false},
		{"*.tinfoil.sh", "router-0.tinfoil.sh.evil.com", false},
		{"*.a.example.com", "x.a.example.com", true},
		{"*.mycorp", "git.mycorp", true}, // internal single-label suffix
		{"*.mycorp", "mycorp", false},
		{"*.com", "example.com", true},
		{"*.a.example.com", "a.example.com", false},

		// any-host
		{"*", "anything.example", true},

		// junk in, no match
		{"", "example.com", false},
		{"example.com", "", false},
	}
	for _, c := range cases {
		if got := HostRuleMatch(c.rule, c.host); got != c.want {
			t.Errorf("HostRuleMatch(%q, %q) = %v, want %v", c.rule, c.host, got, c.want)
		}
	}
}

func TestValidRuleHost(t *testing.T) {
	valid := []string{
		"api.example.com",
		"*.example.com",
		"*.tinfoil.sh",
		"*.a.example.com",
		"*",
		"localhost",
		"worker-demo",
		"*.mycorp",   // internal network, single-label suffix
		"*.localhost",
		"*.com",      // narrower than "*", warned not rejected
	}
	for _, h := range valid {
		if !ValidRuleHost(h) {
			t.Errorf("ValidRuleHost(%q) = false, want true", h)
		}
	}
	invalid := []string{
		"",
		" ",
		"*.",
		"**",
		"*tinfoil.sh",           // missing dot: would match eviltinfoil.sh
		"router-*.tinfoil.sh",   // interior glob: not diffable, rejected
		"api.*.com",             // interior glob
		"*.tinfoil.sh.",         // trailing dot
		"https://example.com",   // scheme
		"api.example.com:8080",  // port lives outside the host pattern
		"api example.com",       // space
		"-example.com",          // leading hyphen
	}
	for _, h := range invalid {
		if ValidRuleHost(h) {
			t.Errorf("ValidRuleHost(%q) = true, want false", h)
		}
	}
}
