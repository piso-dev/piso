package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"piso/cli/internal/pisoconfig"
)

// The opt-out only works if the service actually carries a compose profile:
// COMPOSE_PROFILES= (what dockerComposeEnv exports) is inert otherwise, and
// `piso setup --no-monitor` would keep starting the worker it promises to skip.
func TestGatewayComposeMonitorIsProfiled(t *testing.T) {
	path := filepath.Join("..", "..", "..", "compose", "gateway.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(raw)

	idx := strings.Index(body, "\n  monitor:\n")
	if idx < 0 {
		t.Fatal("compose/gateway.yaml has no monitor service")
	}
	// Only the monitor's own header block matters: take the lines up to the
	// next service (or EOF) and require a profiles key before its image.
	block := body[idx:]
	if end := strings.Index(block[1:], "\n  [a-z0-9_-]+:\n"); end > 0 {
		block = block[:end+1]
	}
	if !strings.Contains(block, `profiles: ["monitor"]`) {
		t.Fatalf("monitor service must declare profiles: [\"monitor\"], got:\n%s", block)
	}
}

// dockerComposeEnv hands this to every compose call, so it has to agree with
// the persisted choice (or the PISO_MONITOR override).
func TestComposeProfilesFollowsMonitorSetting(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	t.Setenv(pisoconfig.MonitorEnvEnv, "")
	if got := pisoconfig.ComposeProfiles(); got != "monitor" {
		t.Fatalf("default ComposeProfiles() = %q, want monitor", got)
	}
	t.Setenv(pisoconfig.MonitorEnvEnv, "0")
	if got := pisoconfig.ComposeProfiles(); got != "" {
		t.Fatalf("PISO_MONITOR=0 ComposeProfiles() = %q, want empty", got)
	}
}

// monitorReason explains WHY the monitor is on or off, so an operator on a
// standalone gateway is never left guessing whether a flag did it.
func TestMonitorReason(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	t.Setenv(pisoconfig.MonitorEnvEnv, "")
	if got := monitorReason(true); got != "" {
		t.Fatalf("monitorReason(true) on a plain host = %q, want empty", got)
	}
	if got := monitorReason(false); got == "" {
		t.Fatal("a disabled monitor must always come with a reason")
	}
	t.Setenv(pisoconfig.MonitorEnvEnv, "0")
	if got := monitorReason(false); !strings.Contains(got, pisoconfig.MonitorEnvEnv) {
		t.Fatalf("monitorReason(false) with %s=0 = %q, want it named", pisoconfig.MonitorEnvEnv, got)
	}
	// An enabled monitor still needs a reason when it is the override: silence
	// there is what makes a standalone host look wrongly configured.
	t.Setenv(pisoconfig.MonitorEnvEnv, "1")
	if got := monitorReason(true); !strings.Contains(got, pisoconfig.MonitorEnvEnv) {
		t.Fatalf("monitorReason(true) with %s=1 = %q, want it named", pisoconfig.MonitorEnvEnv, got)
	}
}

// The image build is the expensive step `piso up` can skip: only run it when
// this host will actually run a piso-worker container.
func TestUpNeedsWorkerImage(t *testing.T) {
	cases := []struct{ standalone, monitor, want bool }{
		{false, true, true},  // laptop: project worker + monitor
		{false, false, true}, // laptop with --no-monitor: still the project worker
		{true, true, true},   // PISO_MONITOR=1 override on a standalone host
		{true, false, false}, // standalone gateway: no worker at all
	}
	for _, c := range cases {
		if got := upNeedsWorkerImage(c.standalone, c.monitor); got != c.want {
			t.Errorf("upNeedsWorkerImage(standalone=%v, monitor=%v) = %v, want %v",
				c.standalone, c.monitor, got, c.want)
		}
	}
}

// A portless public URL means a proxy on :443 forwards to the container, so the
// mux must NOT be published to the host — otherwise the colocated deploy quietly
// reopens 8443 to the internet. An explicit port means direct dials need it.
func TestPublicBindFor(t *testing.T) {
	for publicURL, want := range map[string]string{
		"":                             "127.0.0.1",
		"https://workers.example.com":  "127.0.0.1",
		"https://workers.example.com/": "127.0.0.1",
		"https://203.0.113.9:8443":     "0.0.0.0",
		"https://gw.example.com:8443":  "0.0.0.0",
		"http://10.0.0.5:8443":         "0.0.0.0",
		"not a url":                    "127.0.0.1",
	} {
		if got := publicBindFor(publicURL); got != want {
			t.Errorf("publicBindFor(%q) = %q, want %q", publicURL, got, want)
		}
	}
}
