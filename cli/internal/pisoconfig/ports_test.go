package pisoconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDashboardURLOmitsPort80(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	t.Setenv("PISO_GATEWAY", "")
	if err := saveHostPorts(HostPorts{Proxy: 8080, Control: 80, Ingress: 8082}); err != nil {
		t.Fatal(err)
	}
	if got := DashboardURL(); got != "http://piso.local" {
		t.Fatalf("DashboardURL() = %q", got)
	}
}

func TestDashboardURLIncludesNonDefaultPort(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	t.Setenv("PISO_GATEWAY", "")
	if err := saveHostPorts(HostPorts{Proxy: 8080, Control: 8081, Ingress: 8082}); err != nil {
		t.Fatal(err)
	}
	if got := DashboardURL(); got != "http://piso.local:8081" {
		t.Fatalf("DashboardURL() = %q", got)
	}
}

func TestResolveHostPortsCLIOverridesPersist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PISO_DATA", dir)
	t.Setenv("PISO_PROXY_PORT", "")
	t.Setenv("PISO_CTRL_PORT", "")
	t.Setenv("PISO_INGRESS_PORT", "")
	got, err := ResolveHostPorts(HostPorts{Control: 9091})
	if err != nil {
		t.Fatal(err)
	}
	if got.Control != 9091 || got.Proxy != DefaultProxyPort {
		t.Fatalf("got %+v", got)
	}
	again := LoadHostPorts()
	if again.Control != 9091 {
		t.Fatalf("persist failed: %+v", again)
	}
	if _, err := os.Stat(filepath.Join(dir, "ports.json")); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePortsRejectsCollision(t *testing.T) {
	err := validatePorts(HostPorts{Proxy: 8080, Control: 8080, Ingress: 8082})
	if err == nil {
		t.Fatal("expected collision error")
	}
}

func TestIsUnprobeablePermissionDenied(t *testing.T) {
	err := fmt.Errorf("listen tcp4 127.0.0.1:80: bind: permission denied")
	if !isUnprobeable(err) {
		t.Fatal("permission denied must be unprobeable")
	}
	inUse := fmt.Errorf("listen tcp4 127.0.0.1:80: bind: address already in use")
	if isUnprobeable(inUse) {
		t.Fatal("address already in use must still fail the probe")
	}
}
