package pisoconfig

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Default host-published ports. Control defaults to 80 so the dashboard is
// http://piso.local (no port). In-container listen ports stay 8080/8081/8082
// so the worker's HTTP_PROXY=gateway:8080 does not change.
const (
	DefaultProxyPort          = 8080
	DefaultControlPort        = 80
	DefaultControlPortWindows = 8081
	DefaultIngressPort        = 8082
)

// DashboardHost is the browser hostname for the control-plane UI.
const DashboardHost = "piso.local"

// HostPorts are the localhost publish ports for the gateway.
type HostPorts struct {
	Proxy   int `json:"proxyPort"`
	Control int `json:"ctrlPort"`
	Ingress int `json:"ingressPort"`
}

// DefaultHostPorts returns the built-in mapping. Control is 80 everywhere
// except Windows, where 80 is often reserved (IIS / Hyper-V excluded ranges).
func DefaultHostPorts() HostPorts {
	ctrl := DefaultControlPort
	if runtime.GOOS == "windows" {
		ctrl = DefaultControlPortWindows
	}
	return HostPorts{Proxy: DefaultProxyPort, Control: ctrl, Ingress: DefaultIngressPort}
}

// ResolveHostPorts merges, in order: defaults, ~/.piso/ports.json, process
// env (PISO_*_PORT), then any non-zero CLI overrides. The result is persisted.
func ResolveHostPorts(cli HostPorts) (HostPorts, error) {
	out := DefaultHostPorts()
	if saved, err := loadHostPorts(); err == nil {
		out = mergePorts(out, saved)
	}
	out = mergePorts(out, portsFromEnv())
	out = mergePorts(out, cli)
	if err := validatePorts(out); err != nil {
		return HostPorts{}, err
	}
	if err := saveHostPorts(out); err != nil {
		return HostPorts{}, err
	}
	return out, nil
}

// LoadHostPorts returns persisted ports, or defaults if the file is missing.
func LoadHostPorts() HostPorts {
	if saved, err := loadHostPorts(); err == nil {
		return mergePorts(DefaultHostPorts(), saved)
	}
	return mergePorts(DefaultHostPorts(), portsFromEnv())
}

// ControlAPIURL is what the CLI uses to talk to the control plane (loopback,
// so it works even before /etc/hosts has piso.local).
func ControlAPIURL() string {
	if v := os.Getenv("PISO_GATEWAY"); v != "" {
		return stringsTrimRightSlash(v)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", LoadHostPorts().Control)
}

// DashboardURL is the browser URL: http://piso.local when the control port is
// 80, otherwise http://piso.local:<port>.
func DashboardURL() string {
	if v := os.Getenv("PISO_GATEWAY"); v != "" {
		return stringsTrimRightSlash(v)
	}
	p := LoadHostPorts().Control
	if p == 80 {
		return "http://" + DashboardHost
	}
	return fmt.Sprintf("http://%s:%d", DashboardHost, p)
}

// GatewayURL is an alias of ControlAPIURL (CLI/API, not the pretty hostname).
func GatewayURL() string {
	return ControlAPIURL()
}

// PortInUseError is returned when a chosen host port is already bound.
type PortInUseError struct {
	Name string
	Port int
	Flag string
	Err  error
}

func (e PortInUseError) Error() string {
	return fmt.Sprintf("%s port %d is not available (%v). Set piso up %s <port>", e.Name, e.Port, e.Err, e.Flag)
}

func (e PortInUseError) Unwrap() error { return e.Err }

// CheckHostPortsFree listens on each host port and closes immediately.
// Permission errors are ignored: unprivileged processes cannot bind ports
// below 1024, but Docker Desktop / OrbStack still can publish them.
func CheckHostPortsFree(p HostPorts) error {
	checks := []struct {
		name string
		port int
		flag string
	}{
		{"proxy", p.Proxy, "--proxy-port"},
		{"control", p.Control, "--ctrl-port"},
		{"ingress", p.Ingress, "--ingress-port"},
	}
	for _, c := range checks {
		if err := probePort(c.port); err != nil {
			return PortInUseError{Name: c.name, Port: c.port, Flag: c.flag, Err: err}
		}
	}
	return nil
}

func probePort(port int) error {
	if err := tryListen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err != nil {
		if isUnprobeable(err) {
			return nil
		}
		return err
	}
	err := tryListen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
	if err == nil || isUnusableIPv6(err) || isUnprobeable(err) {
		return nil
	}
	return err
}

func tryListen(network, addr string) error {
	ln, err := net.Listen(network, addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

// isUnprobeable reports a bind the current process is not allowed to make
// (EACCES/EPERM). That is not "port in use": Docker may still publish it.
func isUnprobeable(err error) bool {
	if err == nil {
		return false
	}
	if os.IsPermission(err) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "permission denied") ||
		strings.Contains(s, "operation not permitted")
}

// isUnusableIPv6 reports a machine that cannot bind ::1 at all (not "in use").
func isUnusableIPv6(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "cannot assign") ||
		strings.Contains(s, "protocol not available") ||
		strings.Contains(s, "address family") ||
		strings.Contains(s, "family not supported")
}

func mergePorts(base, over HostPorts) HostPorts {
	if over.Proxy != 0 {
		base.Proxy = over.Proxy
	}
	if over.Control != 0 {
		base.Control = over.Control
	}
	if over.Ingress != 0 {
		base.Ingress = over.Ingress
	}
	return base
}

func portsFromEnv() HostPorts {
	return HostPorts{
		Proxy:   envInt("PISO_PROXY_PORT"),
		Control: envInt("PISO_CTRL_PORT"),
		Ingress: envInt("PISO_INGRESS_PORT"),
	}
}

func envInt(key string) int {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

func validatePorts(p HostPorts) error {
	seen := map[int]string{}
	for name, port := range map[string]int{"proxy": p.Proxy, "control": p.Control, "ingress": p.Ingress} {
		if port < 1 || port > 65535 {
			return fmt.Errorf("%s port %d is out of range", name, port)
		}
		if other, ok := seen[port]; ok {
			return fmt.Errorf("%s and %s cannot share host port %d", other, name, port)
		}
		seen[port] = name
	}
	return nil
}

func portsPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ports.json"), nil
}

func loadHostPorts() (HostPorts, error) {
	path, err := portsPath()
	if err != nil {
		return HostPorts{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return HostPorts{}, err
	}
	var p HostPorts
	if err := json.Unmarshal(b, &p); err != nil {
		return HostPorts{}, err
	}
	return p, nil
}

func saveHostPorts(p HostPorts) error {
	path, err := portsPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func stringsTrimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
