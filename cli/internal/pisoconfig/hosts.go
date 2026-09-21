package pisoconfig

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// HostsPath is the system hosts file. Tests override this.
var HostsPath = defaultHostsPath()

const (
	hostsLineV4 = "127.0.0.1 piso.local"
	hostsLineV6 = "::1 piso.local"
	hostsMarker = "# managed by piso"
)

func defaultHostsPath() string {
	if runtime.GOOS == "windows" {
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		return filepath.Join(root, "System32", "drivers", "etc", "hosts")
	}
	return "/etc/hosts"
}

func hostsEOL() string {
	if runtime.GOOS == "windows" {
		return "\r\n"
	}
	return "\n"
}

func splitHostsLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.Split(text, "\n")
}

func isHostsPermission(err error) bool {
	if err == nil {
		return false
	}
	if os.IsPermission(err) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "access is denied") || strings.Contains(s, "permission denied")
}

// replaceFile writes data to path via a temp file. On Windows os.Rename cannot
// replace an existing dest, so we remove first.
func replaceFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".piso.tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err == nil {
		return nil
	}
	// Windows cannot rename over an existing dest.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

func hostsAddHint() string {
	lines := fmt.Sprintf("Add these lines (once):\n  %s %s\n  %s %s\n",
		hostsLineV4, hostsMarker, hostsLineV6, hostsMarker)
	if runtime.GOOS == "windows" {
		return lines + "Run an elevated PowerShell (`piso sync`), or skip hosts and open the dashboard at 127.0.0.1."
	}
	return lines + fmt.Sprintf("e.g. sudo sh -c 'printf %%s\\\\n %q %q >> %s'",
		hostsLineV4+" "+hostsMarker, hostsLineV6+" "+hostsMarker, HostsPath)
}

// EnsurePisoLocal makes piso.local resolve to 127.0.0.1. If the hosts file is
// not writable, it returns an error that includes the exact lines to add.
func EnsurePisoLocal() error {
	if err := appendPisoLocal(HostsPath); err != nil {
		if isHostsPermission(err) {
			return fmt.Errorf("piso.local does not resolve to loopback and %s is not writable.\n%s",
				HostsPath, hostsAddHint())
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
		if isHostsPermission(err) {
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

// ReconcileIngressHosts rewrites the "# managed by piso" block of /etc/hosts
// so it contains exactly piso.local plus one v4+v6 line per expected ingress
// label. Labels no longer routed are removed (lazy GC — reconcile runs at
// every `piso up` / `piso sync`, re-adding anything a stale removal dropped).
// Non-piso lines are preserved verbatim. When the file is not writable, the
// returned error names the exact lines to add.
func ReconcileIngressHosts(names []string) error {
	return reconcileHosts(HostsPath, names)
}

func reconcileHosts(path string, names []string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	eol := hostsEOL()
	lines := splitHostsLines(string(b))
	// wanted managed entries, keyed "<ip> <fqdn>" (both loopback families)
	want := map[string]bool{}
	want["127.0.0.1 "+DashboardHost] = true
	want["::1 "+DashboardHost] = true
	for _, name := range names {
		fqdn := ingressFQDN(name)
		if fqdn == "" {
			continue
		}
		want["127.0.0.1 "+fqdn] = true
		want["::1 "+fqdn] = true
	}
	var out strings.Builder
	changed := false
	for _, line := range lines {
		if !strings.Contains(line, hostsMarker) {
			out.WriteString(line + eol)
			continue
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && (fields[0] == "127.0.0.1" || fields[0] == "::1") {
			// Keep the entry only when every hostname in it is wanted
			// (comments run to end of line and are ignored).
			all := true
			for j := 1; j < len(fields); j++ {
				if strings.HasPrefix(fields[j], "#") {
					break
				}
				if !want[fields[0]+" "+fields[j]] {
					all = false
					break
				}
			}
			if all {
				out.WriteString(line + eol)
				for j := 1; j < len(fields); j++ {
					if !strings.HasPrefix(fields[j], "#") {
						want[fields[0]+" "+fields[j]] = false
					}
				}
				continue
			}
		}
		changed = true // dropped a stale managed line
	}
	// Append wanted entries that were not present (deterministic order).
	for _, name := range names {
		fqdn := ingressFQDN(name)
		if fqdn == "" {
			continue
		}
		if want["127.0.0.1 "+fqdn] {
			out.WriteString("127.0.0.1 " + fqdn + " " + hostsMarker + eol)
			want["127.0.0.1 "+fqdn] = false
			changed = true
		}
		if want["::1 "+fqdn] {
			out.WriteString("::1 " + fqdn + " " + hostsMarker + eol)
			want["::1 "+fqdn] = false
			changed = true
		}
	}
	if want["127.0.0.1 "+DashboardHost] {
		out.WriteString(hostsLineV4 + " " + hostsMarker + eol)
		changed = true
	}
	if want["::1 "+DashboardHost] {
		out.WriteString(hostsLineV6 + " " + hostsMarker + eol)
		changed = true
	}
	if !changed {
		return nil
	}
	if err := replaceFile(path, []byte(out.String()), 0o644); err != nil {
		if isHostsPermission(err) {
			return fmt.Errorf("%s is not writable; add these lines (once):\n%s", path, ingressHostsHint(names))
		}
		return err
	}
	return nil
}

// ingressHostsHint renders the manual v4+v6 lines for the expected labels.
func ingressHostsHint(names []string) string {
	var b strings.Builder
	for _, name := range names {
		fqdn := ingressFQDN(name)
		if fqdn == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("  127.0.0.1 %s %s\n  ::1 %s %s\n", fqdn, hostsMarker, fqdn, hostsMarker))
	}
	return b.String()
}

func hostsHasName(text, ip, fqdn string) bool {
	for _, line := range splitHostsLines(text) {
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
	eol := hostsEOL()
	var extra strings.Builder
	seen := map[string]bool{}
	for _, name := range names {
		fqdn := ingressFQDN(name)
		if fqdn == "" || seen[fqdn] {
			continue
		}
		seen[fqdn] = true
		if !hostsHasName(text, "127.0.0.1", fqdn) {
			extra.WriteString("127.0.0.1 " + fqdn + " " + hostsMarker + eol)
		}
		if !hostsHasName(text, "::1", fqdn) {
			extra.WriteString("::1 " + fqdn + " " + hostsMarker + eol)
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
	_, err = f.WriteString(eol + extra.String())
	return err
}

func appendPisoLocal(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(b)
	haveV4, haveV6 := false, false
	for _, line := range splitHostsLines(text) {
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
	eol := hostsEOL()
	var bld strings.Builder
	if !haveV4 {
		bld.WriteString(hostsLineV4 + " " + hostsMarker + eol)
	}
	if !haveV6 {
		// macOS treats .local as mDNS; without ::1, IPv6 lookups hang for seconds.
		// The gateway must also publish [::1] or browsers stall on piso.local.
		bld.WriteString(hostsLineV6 + " " + hostsMarker + eol)
	}
	if bld.Len() == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(eol + bld.String())
	return err
}
