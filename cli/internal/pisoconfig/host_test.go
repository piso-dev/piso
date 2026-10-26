package pisoconfig

import (
	"os"
	"testing"

	"piso/internal/technocore"
)

// t.Setenv handles data dir + env override; the file lands in a temp PISO_DATA.
func TestMonitorEnabledDefaultsTrue(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	t.Setenv(MonitorEnvEnv, "")
	if !MonitorEnabled() {
		t.Fatal("monitor must default to enabled so a laptop install is unchanged")
	}
	if got := ComposeProfiles(); got != "monitor" {
		t.Fatalf("ComposeProfiles() = %q, want %q", got, "monitor")
	}
}

func TestSetMonitorEnabledPersists(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	t.Setenv(MonitorEnvEnv, "")

	if _, err := SetMonitorEnabled(false); err != nil {
		t.Fatalf("SetMonitorEnabled(false): %v", err)
	}
	if MonitorEnabled() {
		t.Fatal("monitor should be disabled after SetMonitorEnabled(false)")
	}
	if got := ComposeProfiles(); got != "" {
		t.Fatalf("ComposeProfiles() = %q, want empty when disabled", got)
	}

	// Re-enabling must round-trip through the same file, not reset to default.
	if _, err := SetMonitorEnabled(true); err != nil {
		t.Fatalf("SetMonitorEnabled(true): %v", err)
	}
	if !MonitorEnabled() {
		t.Fatal("monitor should be enabled again")
	}
	if got := ComposeProfiles(); got != "monitor" {
		t.Fatalf("ComposeProfiles() = %q, want %q", got, "monitor")
	}
}

func TestMonitorEnvOverridesFile(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	if _, err := SetMonitorEnabled(true); err != nil {
		t.Fatalf("SetMonitorEnabled(true): %v", err)
	}
	t.Setenv(MonitorEnvEnv, "0")
	if MonitorEnabled() || ComposeProfiles() != "" {
		t.Fatal("PISO_MONITOR=0 must win over the persisted setting")
	}
	t.Setenv(MonitorEnvEnv, "1")
	if !MonitorEnabled() {
		t.Fatal("PISO_MONITOR=1 must win over the persisted setting")
	}
}

func TestParseBoolEnv(t *testing.T) {
	for raw, want := range map[string]bool{
		"1": true, "true": true, "YES": true, " on ": true,
		"0": false, "false": false, "No": false, "off": false,
	} {
		got, ok := parseBoolEnv(raw)
		if !ok || got != want {
			t.Errorf("parseBoolEnv(%q) = %v,%v want %v,true", raw, got, ok, want)
		}
	}
	if _, ok := parseBoolEnv(""); ok {
		t.Error("empty must not parse as a setting")
	}
	if _, ok := parseBoolEnv("maybe"); ok {
		t.Error("garbage must not parse as a setting")
	}
}

// seedGatewayIdentity writes the identity a host has after
// `piso gateway launch --placement <p>` — the file IsStandaloneGatewayHost
// reads. It goes through LoadOrCreate/Save so the key material is real:
// technocore.Load validates the keys and would reject a hand-written stub.
func seedGatewayIdentity(t *testing.T, placement string) {
	t.Helper()
	path := technocore.Path(os.Getenv("PISO_DATA"))
	id, err := technocore.LoadOrCreate(path, "https://api.example.com", "test-gw")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	id.Placement = placement
	id.Status = technocore.StatusActive
	if err := technocore.Save(path, id); err != nil {
		t.Fatalf("save identity: %v", err)
	}
}

// The monitor is a worker: a standalone gateway's host must not run one, even
// with no host.json at all (the case after `piso gateway launch --standalone`).
func TestMonitorDisabledOnStandaloneGatewayHost(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	t.Setenv(MonitorEnvEnv, "")
	seedGatewayIdentity(t, "standalone")

	if !IsStandaloneGatewayHost() {
		t.Fatal("IsStandaloneGatewayHost() should see placement: standalone")
	}
	if MonitorEnabled() {
		t.Fatal("a standalone gateway host must not run the monitor")
	}
	if got := ComposeProfiles(); got != "" {
		t.Fatalf("ComposeProfiles() = %q, want empty on a standalone gateway host", got)
	}
}

// A stale monitor:true from before the promotion must lose to the vault rule —
// otherwise an existing laptop install turns into a worker next to the vault as
// soon as it is promoted.
func TestStandaloneBeatsStaleHostJSON(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	t.Setenv(MonitorEnvEnv, "")
	seedGatewayIdentity(t, "local")
	if _, err := SetMonitorEnabled(true); err != nil {
		t.Fatalf("SetMonitorEnabled(true) on a local host: %v", err)
	}
	seedGatewayIdentity(t, "standalone")
	if MonitorEnabled() {
		t.Fatal("placement: standalone must outrank a persisted monitor:true")
	}
}

// Writing the lie is worse than refusing: host.json would say true while
// MonitorEnabled() keeps answering false.
func TestSetMonitorEnabledRefusedOnStandalone(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	t.Setenv(MonitorEnvEnv, "")
	seedGatewayIdentity(t, "standalone")
	if _, err := SetMonitorEnabled(true); err == nil {
		t.Fatal("enabling the monitor on a standalone gateway host must be refused")
	}
	if MonitorEnabled() {
		t.Fatal("monitor must stay off after a refused enable")
	}
}

// PISO_MONITOR is the documented operator override and outranks the rule; the
// CLI warns when that happens (enforceNoMonitorOnStandalone).
func TestMonitorEnvOverridesStandalone(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	seedGatewayIdentity(t, "standalone")
	t.Setenv(MonitorEnvEnv, "1")
	if !MonitorEnabled() {
		t.Fatal("PISO_MONITOR=1 must remain the explicit override")
	}
}

// Without an identity (fresh host, no `piso gateway launch` yet) nothing may be
// assumed: a plain `piso up` still gets a monitor.
func TestUnenrolledHostKeepsMonitor(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	t.Setenv(MonitorEnvEnv, "")
	if IsStandaloneGatewayHost() || !MonitorEnabled() {
		t.Fatal("an unenrolled host is not a standalone gateway host")
	}
}
