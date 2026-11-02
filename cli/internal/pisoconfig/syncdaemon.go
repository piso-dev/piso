// Package pisoconfig — host-side hosts-sync daemon lifecycle.
//
// The auto-expose loop (worker ports watcher → gateway routes) only becomes
// useful on the HOST when <slug>-<port>.piso.local actually resolves, and
// /etc/hosts needs root to edit. So a privileged background service keeps
// /etc/hosts in step with the gateway's routes:
//
//	piso sync daemon            run the watch loop (as root, forever)
//	piso sync daemon-status     pidfile + ps probe (NO privileges needed)
//	piso sync daemon-restart    privileged: reload the managed service
//	piso sync daemon-stop       privileged: stop it
//	piso sync daemon-uninstall  privileged: stop + remove managed config
//
// The service body is `piso sync daemon`: reconcile on startup, every two
// seconds, and on SSE hints. Stream failures cannot prevent convergence.
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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrSyncDaemonUnsupported is returned on Windows: launchd/nohup are not
// available, and writing the hosts file still needs an elevated `piso sync`.
var ErrSyncDaemonUnsupported = fmt.Errorf("hosts-sync daemon is not supported on Windows; run an elevated `piso sync` after routes change, or add the printed hosts lines manually")

// SyncDaemonSupported is false on Windows (no launchd/nohup service).
func SyncDaemonSupported() bool {
	return runtime.GOOS != "windows"
}

const syncDaemonLabel = "com.piso.sync"
const syncPidFile = "sync.pid"
const syncLogFile = "sync.log"

// SyncDaemonState is the daemon status answer.
type SyncDaemonState struct {
	Running     bool
	Pid         string
	Ready       bool
	LastSuccess time.Time
	LastError   string
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
		result := readSyncResult(dir)
		return SyncDaemonState{Running: true, Pid: pid, Ready: syncResultReady(result, pid, time.Now()), LastSuccess: result.LastSuccess, LastError: result.LastError}, nil
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-p", pid, "-o", "pid=").Output()
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
	return runSyncDaemonOperation("daemon-restart", daemonInstallRestartRoot)
}

// SyncDaemonStop stops the managed service. Idempotent.
func SyncDaemonStop() error {
	if !SyncDaemonSupported() {
		return ErrSyncDaemonUnsupported
	}
	return runSyncDaemonOperation("daemon-stop", daemonStopRoot)
}

// SyncDaemonUninstall stops the service and removes the managed config
// (launchd plist / nohup entry + pidfile). The user's data dir is kept.
func SyncDaemonUninstall() error {
	if !SyncDaemonSupported() {
		return ErrSyncDaemonUnsupported
	}
	return runSyncDaemonOperation("daemon-uninstall", daemonUninstallRoot)
}

// The sudo child performs the entire operation. Its parent must not perform
// it a second time after the child returns successfully.
func runSyncDaemonOperation(op string, rootOperation func() error) error {
	if !amRoot() {
		return ensureRoot(op)
	}
	return rootOperation()
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
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
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
		if err := bootoutSystem(syncDaemonLabel); err != nil {
			return err
		}
	}
	dir, err := DataDir()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, syncStatusFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := writeDaemonConfig(); err != nil {
		return err
	}
	return daemonStartRoot()
}

func daemonStopRoot() error {
	if launchctlAvailable() {
		// bootout is idempotent; a missing label errors, which we swallow.
		return bootoutSystem(syncDaemonLabel)
	}
	// nohup fallback: kill a running daemon via its pidfile.
	return stopNohupDaemon()
}

func daemonUninstallRoot() error {
	if launchctlAvailable() {
		if err := bootoutSystem(syncDaemonLabel); err != nil {
			return err
		}
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
	rmSyncFile(filepath.Join(dir, syncStatusFile))
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

// bootoutSystem stops a loaded service. An explicitly absent service is fine;
// other failures (including timeouts) are surfaced rather than swallowed.
func bootoutSystem(label string) error {
	out, err := runLaunchctl("print", systemServiceTarget(label))
	if err != nil {
		if strings.Contains(out, "Could not find service") || strings.Contains(out, "could not find service") {
			return nil
		}
		return err
	}
	_, err = runLaunchctl("bootout", systemServiceTarget(label))
	return err
}

func runLaunchctl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("launchctl %v: %w", args, ctx.Err())
	}
	if err != nil {
		return string(out), fmt.Errorf("launchctl %v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// daemonStartRoot starts the service once, then waits for a successful hosts
// reconciliation from the new instance. A live PID alone is not readiness.
func daemonStartRoot() error {
	if launchctlAvailable() {
		// RunAtLoad starts it once; do not immediately kill it with kickstart.
		if _, err := runLaunchctl("bootstrap", "system", daemonConfigPath()); err != nil {
			return err
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

// waitForDaemon polls readiness until the deadline (0.3s probes).
func waitForDaemon(seconds int) error {
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(deadline) {
		st, err := SyncDaemonStatus()
		if err == nil && st.Running && st.Ready {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("no successful reconciliation after %ds; see sync daemon-status and sync.log", seconds)
}

// startNohupDaemon executes the nohup bootstrap script (it backgrounds the
// daemon, writes the pidfile, and returns immediately).
func startNohupDaemon() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "/bin/sh", daemonConfigPath()).Run()
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
