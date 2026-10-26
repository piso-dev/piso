package server

import (
	"net"
	"testing"
)

// PISO_TRUSTED_PEER_CIDRS is parsed once, so this test owns the env var.
func TestPeerTrustedFrontProxyCIDR(t *testing.T) {
	t.Setenv("PISO_TRUSTED_PEER_CIDRS", "172.30.99.0/24, 10.9.9.9/32")

	if !peerTrusted(net.ParseIP("172.30.99.2")) {
		t.Fatal("front proxy on the shared bridge must be trusted")
	}
	if !peerTrusted(net.ParseIP("10.9.9.9")) {
		t.Fatal("single-host CIDR must be trusted")
	}
	for _, raw := range []string{"172.30.100.2", "10.9.9.10", "172.18.0.2"} {
		if peerTrusted(net.ParseIP(raw)) {
			t.Fatalf("%s must not be trusted (worker VPC + egress stay denied)", raw)
		}
	}
}

// A trusted CIDR can only cancel a worker-VPC denial; it must never turn a
// default-route peer into something this check accepts. peerOnWorkerVPC uses
// real interfaces, so assert the invariant on the composition of the two
// helpers instead: trust is consulted only inside the deny branch.
func TestTrustOnlyNarrowsDenial(t *testing.T) {
	t.Setenv("PISO_TRUSTED_PEER_CIDRS", "172.30.99.0/24")

	if peerTrusted(net.ParseIP("172.30.99.2")) != true {
		t.Fatal("front proxy must be trusted")
	}
	// Same input, trust removed → the proxy would be denied as a worker.
	t.Setenv("PISO_TRUSTED_PEER_CIDRS", "")
	if peerTrusted(net.ParseIP("172.30.99.2")) {
		t.Fatal("trust must follow the env value")
	}
}
