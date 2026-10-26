package pisoconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"piso/internal/technocore"
)

// hostSettings is $PISO_DATA/host.json — per-host choices that must survive a
// rebuild but are not part of the gateway's own state.json.
//
// Only the monitor lives here today: it is a service in the gateway compose
// project (compose/gateway.yaml), so the only way to keep it off a host is to
// persist the choice and feed COMPOSE_PROFILES on every compose call.
type hostSettings struct {
	// Monitor is a pointer so "absent" (follow placement) is distinguishable
	// from an explicit true/false.
	Monitor *bool `json:"monitor,omitempty"`
}

// MonitorEnvEnv lets an operator force the choice per invocation without
// writing the file: PISO_MONITOR=0/1.
const MonitorEnvEnv = "PISO_MONITOR"

// HostSettingsPath is $PISO_DATA/host.json.
func HostSettingsPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "host.json"), nil
}

func loadHostSettings() (hostSettings, error) {
	path, err := HostSettingsPath()
	if err != nil {
		return hostSettings{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return hostSettings{}, err
	}
	var s hostSettings
	if err := json.Unmarshal(b, &s); err != nil {
		return hostSettings{}, err
	}
	return s, nil
}

// SaveHostSettings writes host.json, preserving fields this CLI does not know.
func saveHostSettings(s hostSettings) error {
	path, err := HostSettingsPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// MonitorEnabled reports whether this host should run the project-manager
// worker. Default true: a plain `piso up` on a laptop keeps working.
//
// A standalone gateway is the exception, and it is a rule rather than a flag: a
// worker must not share a host with the standalone gateway's vault
// (docs/technocore-setup.md), and the monitor is a worker. So placement
// "standalone" turns it off no matter what host.json says — a stale
// monitor:true from before `piso gateway launch --standalone` must not
// resurrect a worker next to the vault.
//
// PISO_MONITOR still wins over everything: it is the documented, explicit
// operator override, and silently ignoring it would be worse than the mistake
// it asks for.
func MonitorEnabled() bool {
	if v, ok := parseBoolEnv(os.Getenv(MonitorEnvEnv)); ok {
		return v
	}
	if s, err := loadHostSettings(); err == nil && s.Monitor != nil && !*s.Monitor {
		return false
	}
	// Default on, and an explicit true cannot outrank the vault rule below.
	return !IsStandaloneGatewayHost()
}

// IsStandaloneGatewayHost reports whether this host's enrolled gateway identity
// says placement: standalone. Unenrolled (or unreadable) is NOT standalone, so
// a plain laptop or a half-set-up host still gets a monitor.
func IsStandaloneGatewayHost() bool {
	dir, err := DataDir()
	if err != nil {
		return false
	}
	id, err := technocore.Load(technocore.Path(dir))
	if err != nil {
		return false
	}
	return id.PlacementOrLocal() == technocore.PlacementStandalone
}

// MonitorEnvValue reports an explicit PISO_MONITOR setting, if any, so a
// one-shot `PISO_MONITOR=0 make install` can be made sticky instead of
// resurrecting the monitor on the next plain `piso setup --rebuild`.
func MonitorEnvValue() (bool, bool) {
	return parseBoolEnv(os.Getenv(MonitorEnvEnv))
}

// SetMonitorEnabled persists the choice and returns the effective value, so a
// PISO_MONITOR override is reported rather than silently contradicting it.
//
// Enabling on a standalone gateway host is refused instead of written: the
// file would say "monitor: true" while MonitorEnabled() keeps answering false,
// and the operator would be left reading a lie on disk.
func SetMonitorEnabled(v bool) (bool, error) {
	if v && IsStandaloneGatewayHost() {
		return false, errors.New("this host is a standalone gateway (placement: standalone); " +
			"its worker must not run next to the vault — the monitor stays off")
	}
	s, err := loadHostSettings()
	if err != nil {
		s = hostSettings{}
	}
	set := v
	s.Monitor = &set
	if err := saveHostSettings(s); err != nil {
		return MonitorEnabled(), err
	}
	return MonitorEnabled(), nil
}

// ComposeProfiles is the COMPOSE_PROFILES value for the gateway compose
// project: "monitor" when the project-manager service should start, "" when it
// must be left out. Docker Compose skips services carrying a profile that is
// not enabled, so the monitor's own `profiles: [monitor]` is what makes this
// work without a second compose file.
func ComposeProfiles() string {
	if MonitorEnabled() {
		return "monitor"
	}
	return ""
}

func parseBoolEnv(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}
