package dockernet

import (
	"errors"
	"strings"
	"testing"
)

func TestParseInternal(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"true", true},
		{"True\n", true},
		{"false", false},
		{"", false},
		{"  false  ", false},
	}
	for _, tc := range cases {
		if got := parseInternal(tc.in); got != tc.want {
			t.Fatalf("parseInternal(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsMissingNetwork(t *testing.T) {
	err := errors.New("exit status 1")
	if !isMissingNetwork("Error: No such network: piso_vpc", err) {
		t.Fatal("expected missing-network match")
	}
	if !isMissingNetwork("Error response from daemon: network piso_vpc not found", err) {
		t.Fatal("expected OrbStack/engine 'not found' match")
	}
	if isMissingNetwork("permission denied", err) {
		t.Fatal("did not expect missing-network match")
	}
	if isMissingNetwork("Cannot connect to the Docker daemon", err) {
		t.Fatal("daemon-down is not a missing network")
	}
	if isMissingNetwork("", nil) {
		t.Fatal("nil error is not missing")
	}
}

func TestDockerCandidatesWindows(t *testing.T) {
	got := dockerCandidates("windows", `C:\Users\x`, `C:\Program Files`)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "docker.exe") {
		t.Fatalf("expected docker.exe, got %v", got)
	}
	if strings.Contains(joined, "orbstack") {
		t.Fatal("orbstack should not be a windows candidate")
	}
}

func TestDockerCandidatesUnix(t *testing.T) {
	got := dockerCandidates("linux", "/home/u", "")
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "orbstack") {
		t.Fatalf("expected orbstack candidate, got %v", got)
	}
	if strings.Contains(joined, "docker.exe") {
		t.Fatal("docker.exe should not be a unix candidate")
	}
}

func TestParseContainerIPs(t *testing.T) {
	got := parseContainerIPs("vpc 192.168.107.50 egress 172.20.0.2 ")
	if len(got) != 2 || got[0] != "192.168.107.50" || got[1] != "172.20.0.2" {
		t.Fatalf("parse %+v", got)
	}
	if len(parseContainerIPs("")) != 0 {
		t.Fatal("expected empty")
	}
	if len(parseContainerIPs("bzzz somehost foo")) != 0 {
		t.Fatal("hostnames should be rejected")
	}
}

