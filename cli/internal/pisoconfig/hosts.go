package pisoconfig

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// HostsPath is /etc/hosts. Tests override this.
var HostsPath = "/etc/hosts"

const (
	hostsLineV4 = "127.0.0.1 piso.local"
	hostsLineV6 = "::1 piso.local"
	hostsMarker = "# managed by piso"
)

// EnsurePisoLocal makes piso.local resolve to 127.0.0.1. If /etc/hosts is not
// writable, it returns an error that includes the exact line to add.
func EnsurePisoLocal() error {
	if err := appendPisoLocal(HostsPath); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("piso.local does not resolve to loopback and %s is not writable.\nAdd these lines (once):\n  %s %s\n  %s %s\ne.g. sudo sh -c 'printf %%s\\\\n %q %q >> %s'",
				HostsPath, hostsLineV4, hostsMarker, hostsLineV6, hostsMarker,
				hostsLineV4+" "+hostsMarker, hostsLineV6+" "+hostsMarker, HostsPath)
		}
		return err
	}
	if !pisoLocalIsLoopback() {
		return fmt.Errorf("wrote %s but piso.local still does not resolve to 127.0.0.1 (flush DNS / check another piso.local entry)", HostsPath)
	}
	return nil
}

func pisoLocalIsLoopback() bool {
	ips, err := net.LookupHost(DashboardHost)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		if parsed != nil && parsed.IsLoopback() {
			return true
		}
	}
	return false
}

// EnsureIngressHost adds name.piso.local → loopback. /etc/hosts has no
// wildcards, so each ingress label needs its own line.
func EnsureIngressHost(name string) error {
	return EnsureIngressHosts([]string{name})
}

// EnsureIngressHosts adds each name.piso.local to /etc/hosts when missing.
func EnsureIngressHosts(names []string) error {
	if err := appendIngressHosts(HostsPath, names); err != nil {
		if os.IsPermission(err) {
			var hint strings.Builder
			for _, name := range names {
				fqdn := ingressFQDN(name)
				if fqdn == "" {
					continue
				}
				hint.WriteString(fmt.Sprintf("  127.0.0.1 %s %s\n  ::1 %s %s\n", fqdn, hostsMarker, fqdn, hostsMarker))
			}
			return fmt.Errorf("%s is not writable; add:\n%s", HostsPath, hint.String())
		}
		return err
	}
	return nil
}

func ingressFQDN(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || name == DashboardHost || strings.Contains(name, ".") {
		return ""
	}
	for i, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '-' && i > 0)
		if !ok {
			return ""
		}
	}
	if strings.HasSuffix(name, "-") {
		return ""
	}
	return name + "." + DashboardHost
}

func hostsHasName(text, ip, fqdn string) bool {
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "#") {
			continue
		}
		fields := strings.Fields(trim)
		if len(fields) < 2 || fields[0] != ip {
			continue
		}
		for _, n := range fields[1:] {
			if n == fqdn {
				return true
			}
		}
	}
	return false
}

func appendIngressHosts(path string, names []string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(b)
	var extra strings.Builder
	seen := map[string]bool{}
	for _, name := range names {
		fqdn := ingressFQDN(name)
		if fqdn == "" || seen[fqdn] {
			continue
		}
		seen[fqdn] = true
		if !hostsHasName(text, "127.0.0.1", fqdn) {
			extra.WriteString("127.0.0.1 " + fqdn + " " + hostsMarker + "\n")
		}
		if !hostsHasName(text, "::1", fqdn) {
			extra.WriteString("::1 " + fqdn + " " + hostsMarker + "\n")
		}
	}
	if extra.Len() == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n" + extra.String())
	return err
}

func appendPisoLocal(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(b)
	haveV4, haveV6 := false, false
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "#") {
			continue
		}
		fields := strings.Fields(trim)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "127.0.0.1" || fields[0] == "::1" {
			for _, name := range fields[1:] {
				if name == DashboardHost {
					if fields[0] == "127.0.0.1" {
						haveV4 = true
					} else {
						haveV6 = true
					}
				}
			}
		}
	}
	var bld strings.Builder
	if !haveV4 {
		bld.WriteString(hostsLineV4 + " " + hostsMarker + "\n")
	}
	if !haveV6 {
		// macOS treats .local as mDNS; without ::1, IPv6 lookups hang for seconds.
		// The gateway must also publish [::1] or browsers stall on piso.local.
		bld.WriteString(hostsLineV6 + " " + hostsMarker + "\n")
	}
	if bld.Len() == 0 {
		return nil
	}
	entry := "\n" + bld.String()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(entry)
	return err
}
