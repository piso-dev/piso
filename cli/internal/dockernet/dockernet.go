// Package dockernet inspects and repairs the shared piso Docker networks.
// Compose will not flip Internal on an already-created bridge, so `piso up`
// must tear down a leaky piso_vpc before recreating it from gateway.yaml.
package dockernet

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Network names must match compose/gateway.yaml (`name:` pins).
const (
	VPCName    = "piso_vpc"
	EgressName = "piso_egress"
)

// InspectVPC reports whether piso_vpc exists and whether Docker marked it
// internal (off-bridge forwarding dropped). A missing network is not an error:
// compose up will create it from gateway.yaml.
func InspectVPC() (exists bool, internal bool, err error) {
	out, runErr := dockerOutput("network", "inspect", VPCName, "--format", "{{.Internal}}")
	if runErr != nil {
		if isMissingNetwork(out, runErr) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("inspect %s: %s: %w", VPCName, out, runErr)
	}
	return true, parseInternal(out), nil
}

// EnsureInternal tears down piso containers and networks when piso_vpc exists
// but is not internal. A missing network is left for compose to create.
func EnsureInternal() error {
	exists, internal, err := InspectVPC()
	if err != nil {
		return err
	}
	if !exists || internal {
		return nil
	}
	fmt.Printf("piso: %s is not internal — tearing down piso containers and networks\n", VPCName)
	return RemoveRuntime()
}

// AssertInternal fails if piso_vpc is missing or not internal. Call after
// gateway compose up so a leftover leaky network cannot silently survive.
func AssertInternal() error {
	exists, internal, err := InspectVPC()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s is missing after compose up", VPCName)
	}
	if !internal {
		return fmt.Errorf("%s is not internal (workers can bypass the gateway)", VPCName)
	}
	return nil
}

// RemoveRuntime force-removes piso-gateway / piso-worker-* containers and
// the vpc/egress networks. Safe during testing; nothing is migrated.
func RemoveRuntime() error {
	idsOut, err := dockerOutput("ps", "-aq", "--filter", "name=piso-")
	if err != nil {
		return fmt.Errorf("list piso containers: %s: %w", idsOut, err)
	}
	ids := strings.Fields(idsOut)
	if len(ids) > 0 {
		args := append([]string{"rm", "-f"}, ids...)
		if out, rmErr := dockerOutput(args...); rmErr != nil {
			return fmt.Errorf("remove piso containers: %s: %w", out, rmErr)
		}
	}
	for _, name := range []string{VPCName, EgressName} {
		out, rmErr := dockerOutput("network", "rm", name)
		if rmErr != nil && !isMissingNetwork(out, rmErr) {
			return fmt.Errorf("remove network %s: %s: %w", name, out, rmErr)
		}
	}
	return nil
}

// parseInternal interprets `docker network inspect --format {{.Internal}}`.
func parseInternal(out string) bool {
	return strings.EqualFold(strings.TrimSpace(out), "true")
}

// isMissingNetwork reports a docker inspect/rm "No such network" failure.
func isMissingNetwork(out string, err error) bool {
	if err == nil {
		return false
	}
	blob := strings.ToLower(out + " " + err.Error())
	return strings.Contains(blob, "no such network")
}

// LookPath finds the docker CLI. `sudo make install` drops privileges with a
// thin PATH, so we also probe common Desktop / OrbStack locations.
func LookPath() (string, error) {
	if p, err := exec.LookPath("docker"); err == nil {
		return p, nil
	}
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".orbstack", "bin", "docker"),
			filepath.Join(home, ".docker", "bin", "docker"),
		)
	}
	candidates = append(candidates,
		"/usr/local/bin/docker",
		"/opt/homebrew/bin/docker",
		"/usr/bin/docker",
	)
	for _, c := range candidates {
		info, err := os.Stat(c)
		if err != nil || info.IsDir() {
			continue
		}
		return c, nil
	}
	return "", fmt.Errorf("docker not found in PATH; install Docker Desktop or OrbStack")
}

// dockerOutput runs `docker args...` and returns trimmed combined output.
func dockerOutput(args ...string) (string, error) {
	bin, err := LookPath()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ContainerIPs returns the IPv4/v6 addresses the container is attached to,
// parsed from `docker inspect`. Used to populate the worker registry (slug↔IP)
// so the gateway can tag request logs.
func ContainerIPs(name string) ([]string, error) {
	out, err := dockerOutput("inspect", "-f", "{{range $n, $conf := .NetworkSettings.Networks}}{{$n}} {{$conf.IPAddress}} {{end}}", name)
	if err != nil {
		return nil, err
	}
	return parseContainerIPs(string(out)), nil
}

// parseContainerIPs extracts the IP tokens from `docker inspect` output that is
// a space-separated sequence of "<netname> <ip>" pairs. A token is an IP if it
// contains only IP characters (digits, dots, colons, hex letters); the network
// name tokens (e.g. "bzzz", "vpc") are discarded.
func parseContainerIPs(out string) []string {
	var ips []string
	for _, line := range strings.Split(out, " ") {
		if isIPString(line) {
			ips = append(ips, line)
		}
	}
	return ips
}

// isIPString reports a token that looks like a literal IPv4/IPv6 address:
// digits, dots, colons, or hex letters. Hostnames with letters beyond a-f are
// rejected (so network names like "vpc" / "bzzz" are not treated as IPs).
func isIPString(s string) bool {
	if s == "" || len(s) > 45 {
		return false
	}
	digits := 0
	for _, r := range strings.ToLower(s) {
		digit := r >= '0' && r <= '9'
		hex := r >= 'a' && r <= 'f'
		sep := r == '.' || r == ':' || r == '_'
		if !digit && !hex && !sep {
			return false
		}
		if digit {
			digits++
		}
	}
	return digits > 0
}
