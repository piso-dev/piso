package main

import "testing"

func TestProxyNetworkCIDRs(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		bad       bool
	}{
		{"172.30.99.0/24\n", "172.30.99.0/24", false},
		{"10.0.0.2/24\nfd00::/64", "10.0.0.0/24,fd00::/64", false},
		{"", "", true}, {"not-a-subnet", "", true},
	} {
		got, err := proxyNetworkCIDRs(tc.raw)
		if (err != nil) != tc.bad || got != tc.want {
			t.Errorf("parse %q: %q, %v", tc.raw, got, err)
		}
	}
}
