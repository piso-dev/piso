package pisoconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// daemonTestDir points PISO_DATA at a temp dir so all daemon paths resolve
// inside it.
func daemonTestDir(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("PISO_DATA", dir)
	return dir
}

func TestSyncDaemonSupported(t *testing.T) {
	if SyncDaemonSupported() == (runtime.GOOS == "windows") {
		t.Fatalf("SyncDaemonSupported()=%v on %s", SyncDaemonSupported(), runtime.GOOS)
	}
}

func TestSyncDaemonStatusStoppedWithoutPidfile(t *testing.T) {
	daemonTestDir(t)
	st, err := SyncDaemonStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Running || st.Pid != "" {
		t.Fatalf("expected stopped, got %+v", st)
	}
}

func TestSyncDaemonStatusStalePidfile(t *testing.T) {
	dir := daemonTestDir(t)
	if err := os.WriteFile(filepath.Join(dir, "sync.pid"), []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := SyncDaemonStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Running {
		t.Fatal("bogus pid must not read as running")
	}
}

func TestSyncDaemonPlistContent(t *testing.T) {
	plist := syncDaemonPlist("/usr/local/bin/piso", "/Users/u/.piso")
	txt := string(plist)
	for _, want := range []string{
		"com.piso.sync",
		"/usr/local/bin/piso",
		"/Users/u/.piso",
		"RunAtLoad",
		"KeepAlive",
		"sync daemon",
		"sync.pid",
		"sync.log",
	} {
		if !strings.Contains(txt, want) {
			t.Fatalf("plist missing %q:\n%s", want, txt)
		}
	}
}

func TestSyncDaemonNohupBootstrapWritesPid(t *testing.T) {
	// The bootstrap must kill a live stale daemon then restart it with a new
	// pidfile — the launcher that matters (echo $! > pidfile, PISO_DATA baked).
	boot := nohupBootstrap("/usr/local/bin/piso", "/Users/u/.piso")
	txt := string(boot)
	for _, want := range []string{
		"nohup",
		"/usr/local/bin/piso",
		"sync.pid",
		"sync.log",
		"PISO_DATA=",
		"echo $!",
	} {
		if !strings.Contains(txt, want) {
			t.Fatalf("bootstrap missing %q:\n%s", want, txt)
		}
	}
}

func TestSyncDaemonPlistLabelAppearsOnce(t *testing.T) {
	// launchctl detection drives config path + manager selection; sanity: the
	// renderer produces the label once so the file is findable by label.
	plist := syncDaemonPlist("/opt/homebrew/bin/piso", "/Users/u/.piso")
	if strings.Count(string(plist), "com.piso.sync") != 1 {
		t.Fatalf("label should appear exactly once\n%s", plist)
	}
}
// Regression guard: launchctl target for a LaunchDaemon must be domain-
// qualified (system/<label>). Without it, kickstart/bootout resolve in the
// user domain and exit 64 (the original failure). We assert the string the
// launchctl layer builds so a future edit can't reintroduce the bug without
// the test noticing. (The exec commands themselves can't run in CI here —
// launchctl is host-only — so we lock the string, not the side effect.)
func TestSyncDaemonLaunchctlUsesSystemDomain(t *testing.T) {
	got := systemServiceTarget(syncDaemonLabel)
	if got != "system/com.piso.sync" {
		t.Fatalf("service target %q", got)
	}
	// The prefix is the whole point: without system/, kickstart/bootout
	// resolve in the user (gui/$UID) domain and exit 64.
	if !strings.HasPrefix(got, "system/") {
		t.Fatalf("target not system-domain qualified: %q", got)
	}
}
