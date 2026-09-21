// Package pisoconfig — host-side hosts-sync daemon lifecycle.
//
// The auto-expose loop (worker ports watcher → gateway routes) only becomes
// useful on the HOST when <slug>-<port>.piso.local actually resolves, and
// /etc/hosts needs root to edit. So a privileged background service keeps
// /etc/hosts in step with the gateway's routes:
//
//   piso sync daemon            run the watch loop (as root, forever)
//   piso sync daemon-status     pidfile + ps probe (NO privileges needed)
//   piso sync daemon-restart    privileged: reload the managed service
//   piso sync daemon-stop       privileged: stop it
//   piso sync daemon-uninstall  privileged: stop + remove managed config
//
// The service body is `piso sync daemon` — the same SSE watch loop as
// `piso sync --watch`, which reconciles /etc/hosts whenever a route event
// arrives (new auto route, route deleted, stale route swept).
//
// Manager selection: launchd on macOS (system domain, RunAtLoad + KeepAlive,
// so it survives reboots and crashes); a nohup + pidfile fallback elsewhere
// (best-effort; no auto-restart).
//
// Privileges: the privileged verbs re-exec themselves via `sudo
// /usr/bin/env PISO_DATA=<dir> <piso> sync daemon-<op>`. PISO_DATA is threaded
// through explicitly because sudo strips the environment and the daemon must
// keep using the USER's data dir (ports.json, pidfile, log) even though it
// runs as root.
package pisoconfig

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrSyncDaemonUnsupported is returned on Windows: launchd/nohup are not
// available, and writing the hosts file still needs an elevated `piso sync`.
var ErrSyncDaemonUnsupported = fmt.Errorf("hosts-sync daemon is not supported on Windows; run an elevated `piso sync` after routes change, or add the printed hosts lines manually")

// SyncDaemonSupported is false on Windows (no launchd/nohup service).
func SyncDaemonSupported() bool {
	return runtime.GOOS != "windows"
}

const syncDaemonLabel = "com.piso.sync"
const syncPidFile    = "sync.pid"
const syncLogFile    = "sync.log"

// SyncDaemonState is the daemon status answer.
type SyncDaemonState struct {
	Running bool
	Pid     string
}

// DaemonStatus checks the pidfile + liveness WITHOUT root: the pidfile lives
// in the user's data dir (readable) and a process is probed with `ps`, which
// needs no elevated rights. Fails → not running (stale pidfile).
func SyncDaemonStatus() (SyncDaemonState, error) {
	dir, err := DataDir()
	if err != nil {
		return SyncDaemonState{}, err
	}
	pid := readPidFile(filepath.Join(dir, syncPidFile))
	if pid == "" {
		return SyncDaemonState{}, nil
	}
	if daemonAlive(pid) {
		return SyncDaemonState{Running: true, Pid: pid}, nil
	}
	// stale pidfile: not running
	return SyncDaemonState{}, nil
}

func readPidFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// daemonAlive probes the pid with `ps` (exists on macOS and Linux; the worker
// image is not the target host). ps -p <pid> -o pid= prints the pid iff the
// process exists.
func daemonAlive(pid string) bool {
	out, err := exec.Command("ps", "-p", pid, "-o", "pid=").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == pid
}

// SyncDaemonInstallRestart installs (or refreshes) the managed service and
// starts it. Idempotent: an already-running daemon is replaced. Escalates to
// root for the privileged parts.
func SyncDaemonInstallRestart() error {
	if !SyncDaemonSupported() {
		return ErrSyncDaemonUnsupported
	}
	if err := ensureRoot("daemon-restart"); err != nil {
		return err
	}
	return daemonInstallRestartRoot()
}

// SyncDaemonStop stops the managed service. Idempotent.
func SyncDaemonStop() error {
	if !SyncDaemonSupported() {
		return ErrSyncDaemonUnsupported
	}
	if err := ensureRoot("daemon-stop"); err != nil {
		return err
	}
	return daemonStopRoot()
}

// SyncDaemonUninstall stops the service and removes the managed config
// (launchd plist / nohup entry + pidfile). The user's data dir is kept.
func SyncDaemonUninstall() error {
	if !SyncDaemonSupported() {
		return ErrSyncDaemonUnsupported
	}
	if err := ensureRoot("daemon-uninstall"); err != nil {
		return err
	}
	return daemonUninstallRoot()
}

// ensureRoot re-runs this same command through `sudo /usr/bin/env
// PISO_DATA=<dataDir> <self> sync <op>` so the privileged code runs as root
// with the USER's data dir preserved. Already-root callers (e.g. `sudo make
// install`) proceed directly; the sudo child is root and does not re-loop.
func ensureRoot(op string) error {
	if amRoot() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := DataDir()
	if err != nil {
		return err
	}
	c := exec.Command("sudo", "/usr/bin/env", "PISO_DATA="+dir, exe, "sync", op)
	if rerr := c.Run(); rerr != nil {
		return fmt.Errorf("need root to %s. Run manually:\n  sudo /usr/bin/env PISO_DATA=%s %s sync %s",
			op, dir, exe, op)
	}
	return nil
}

// amRoot reports whether we can write /etc/hosts (uid 0). `id -u` exists on
// macOS and Linux and works without a special API.
func amRoot() bool {
	out, err := exec.Command("id", "-u").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "0"
}

// daemonInstallRestartRoot is the privileged install/restart. It unloads any
// existing instance (bootout, ignoring "not loaded"), (re)writes the managed
// config, then starts it — so an upgrade cleanly replaces the old instance.
func daemonInstallRestartRoot() error {
	if launchctlAvailable() {
		bootoutSystem(syncDaemonLabel)
	}
	if err := writeDaemonConfig(); err != nil {
		return err
	}
	return daemonStartRoot()
}

func daemonStopRoot() error {
	if launchctlAvailable() {
		// bootout is idempotent; a missing label errors, which we swallow.
		bootoutSystem(syncDaemonLabel)
		return nil
	}
	// nohup fallback: kill a running daemon via its pidfile.
	return stopNohupDaemon()
}

func daemonUninstallRoot() error {
	if launchctlAvailable() {
		bootoutSystem(syncDaemonLabel)
		return removeManagedFiles()
	}
	if err := stopNohupDaemon(); err != nil {
		return err
	}
	return removeManagedFiles()
}

func removeManagedFiles() error {
	dir, err := DataDir()
	if err != nil {
		return err
	}
	rmSyncFile(daemonConfigPath())
	rmSyncFile(filepath.Join(dir, syncPidFile))
	return nil
}

func rmSyncFile(path string) {
	if path == "" {
		return
	}
	c := exec.Command("/bin/rm", "-f", path)
	_ = c.Run()
}

// systemServiceTarget is the launchctl target for the system-domain daemon.
// MUST stay "system/<label>": kickstart/bootout without the domain prefix
// resolve in the user (gui/$UID) domain and exit 64.
func systemServiceTarget(label string) string {
	return "system/" + label
}

// bootoutSystem stops the system-domain service by label. Errors (e.g. "no
// such service") are swallowed — it is called defensively before install and
// by stop/uninstall where an absent daemon is fine.
func bootoutSystem(label string) {
	_ = exec.Command("launchctl", "bootout", systemServiceTarget(label)).Run()
}

// daemonStartRoot starts (or restarts) the managed service, then VERIFIES the
// daemon actually comes up. launchctl bootstrap/kickstart return once launchd
// *queues* the start — the process may still be relaunching — so a bare exit
// code is a flaky signal (the original `sudo make install` warning was this
// race). We poll piso's own daemon-status until it reports running.
func daemonStartRoot() error {
	if launchctlAvailable() {
		// bootstrap loads the plist into the system domain (RunAtLoad starts
		// it). Errors are swallowed: on an upgrade a just-issued bootout can
		// still be draining, making bootstrap exit "already loaded".
		_ = exec.Command("launchctl", "bootstrap", "system", daemonConfigPath()).Run()
		// kickstart -k kills + restarts the service so it picks up the new
		// binary. MUST be domain-qualified (system/<label>) or it resolves in
		// the user domain and exits 64.
		c := exec.Command("launchctl", "kickstart", "-k", systemServiceTarget(syncDaemonLabel))
		if err := c.Run(); err != nil {
			// launchd queued but did not accept; try waiting anyway — the
			// process may still come up from bootstrap alone.
			_ = err
		}
	} else if err := startNohupDaemon(); err != nil {
		return err
	}
	// Wait for the daemon to actually report running (launchd relaunch or
	// fresh boot). Give it a few seconds; the pidfile+ps probe is cheap.
	if err := waitForDaemon(20); err != nil {
		return fmt.Errorf("sync daemon did not come up: %v", err)
	}
	return nil
}

// waitForDaemon polls piso's daemon-status until the daemon reports running or
// ~seconds elapse (0.3s probes).
func waitForDaemon(seconds int) error {
	for i := 0; i < seconds*3; i++ {
		st, err := SyncDaemonStatus()
		if err == nil && st.Running {
			return nil
		}
		exec.Command("/bin/sleep", "0.3").Run()
	}
	return fmt.Errorf("not running after %ds", seconds)
}

// startNohupDaemon executes the nohup bootstrap script (it backgrounds the
// daemon, writes the pidfile, and returns immediately).
func startNohupDaemon() error {
	return exec.Command("/bin/sh", daemonConfigPath()).Run()
}

// writeDaemonConfig writes the launchd plist (/Library/LaunchDaemons) or, on
// hosts without launchctl, the nohup bootstrap script next to the data dir.
func writeDaemonConfig() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := DataDir()
	if err != nil {
		return err
	}
	if launchctlAvailable() {
		plist := syncDaemonPlist(exe, dir)
		return os.WriteFile(daemonConfigPath(), []byte(plist), 0o644)
	}
	return os.WriteFile(daemonConfigPath(), []byte(nohupBootstrap(exe, dir)), 0o755)
}

// daemonConfigPath is the managed config file location.
func daemonConfigPath() string {
	if launchctlAvailable() {
		return "/Library/LaunchDaemons/" + syncDaemonLabel + ".plist"
	}
	dir, _ := DataDir()
	return filepath.Join(dir, "sync-daemon.sh")
}

func launchctlAvailable() bool {
	_, err := exec.LookPath("launchctl")
	return err == nil
}

// syncDaemonPlist renders the launchd service definition. The wrapper writes
// the daemon's PID to <dir>/sync.pid BEFORE exec'ing piso: $$ is the sh pid
// and survives the exec, so the pidfile always names the live daemon process.
// PISO_DATA is baked in because launchd passes a minimal environment.
func syncDaemonPlist(exe, dir string) string {
	pidPath := filepath.Join(dir, syncPidFile)
	logPath := filepath.Join(dir, syncLogFile)
	inner := fmt.Sprintf("echo $$ > '%s'; exec '%s' sync daemon", pidPath, exe)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>%s</string>
	</array>
	<key>WorkingDirectory</key><string>/</string>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PISO_DATA</key><string>%s</string>
		<key>PATH</key><string>/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin</string>
	</dict>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>`, syncDaemonLabel, inner, dir, logPath, logPath)
}

// nohupBootstrap is the non-launchd fallback: a small script that (re)starts
// the watch loop detached with a pidfile. Run by root via the Makefile path.
func nohupBootstrap(exe, dir string) string {
	pidPath := filepath.Join(dir, syncPidFile)
	logPath := filepath.Join(dir, syncLogFile)
	return fmt.Sprintf(`#!/bin/sh
# piso hosts-sync daemon (nohup fallback) — regenerated by piso sync daemon-restart
pidf="%s"
logf="%s"
[ -f "$pidf" ] && kill -0 "$(cat "$pidf")" 2>/dev/null && kill "$(cat "$pidf")" 2>/dev/null
sleep 0.2
cd /
PISO_DATA="%s" nohup '%s' sync daemon >> "$logf" 2>&1 < /dev/null &
echo $! > "$pidf"
`, pidPath, logPath, dir, exe)
}

// stopNohupDaemon kills the daemon named by the pidfile.
func stopNohupDaemon() error {
	dir, err := DataDir()
	if err != nil {
		return err
	}
	pid := readPidFile(filepath.Join(dir, syncPidFile))
	if pid == "" {
		return nil
	}
	c := exec.Command("kill", pid)
	_ = c.Run()
	rmSyncFile(filepath.Join(dir, syncPidFile))
	return nil
}