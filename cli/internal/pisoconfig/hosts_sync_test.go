package pisoconfig

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHostsSnapshotLoadedInsideLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0644)
	old := HostsPath
	HostsPath = path
	defer func() { HostsPath = old }()
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 2)
	go func() {
		finished <- withHostsLock(path, func() error { close(entered); <-release; return appendIngressHostsUnlocked(path, []string{"first"}) })
	}()
	<-entered
	loaded := make(chan struct{})
	go func() {
		finished <- ReconcileIngressHostsFrom(func() ([]string, error) { close(loaded); return []string{"first", "second"}, nil })
	}()
	select {
	case <-loaded:
		t.Fatal("snapshot fetched outside host-wide lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(path)
	for _, name := range []string{"localhost", "first.piso.local", "second.piso.local"} {
		if !strings.Contains(string(b), name) {
			t.Fatalf("lost %s", name)
		}
	}
	info, _ := os.Stat(path)
	time.Sleep(10 * time.Millisecond)
	if err := ReconcileIngressHosts([]string{"first", "second"}); err != nil {
		t.Fatal(err)
	}
	next, _ := os.Stat(path)
	if !info.ModTime().Equal(next.ModTime()) {
		t.Fatal("no-op reconcile rewrote hosts")
	}
}

func TestSyncStatusRequiresSuccessfulCurrentInstance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("daemon is unsupported on Windows")
	}
	// Isolate the process probe: CI workers need not have ps installed.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\nprintf '%s\\n' \"$2\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := daemonTestDir(t)
	pid := strconv.Itoa(os.Getpid())
	os.WriteFile(filepath.Join(dir, syncPidFile), []byte(pid), 0644)
	state, err := SyncDaemonStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !state.Running || state.Ready {
		t.Fatalf("PID alone is not ready: %+v", state)
	}
	if err := RecordSyncResult(nil); err != nil {
		t.Fatal(err)
	}
	state, _ = SyncDaemonStatus()
	if !state.Ready {
		t.Fatalf("successful sync not ready: %+v", state)
	}
	if err := RecordSyncResult(errors.New("gateway down")); err != nil {
		t.Fatal(err)
	}
	state, _ = SyncDaemonStatus()
	if state.Ready || state.LastError != "gateway down" || state.LastSuccess.IsZero() {
		t.Fatalf("error status: %+v", state)
	}
	result := readSyncResult(dir)
	result.LastError = ""
	if syncResultReady(result, "other-pid", time.Now()) {
		t.Fatal("stale instance blessed restart")
	}
	if syncResultReady(result, pid, time.Now().Add(time.Minute)) {
		t.Fatal("stalled daemon still ready")
	}
}
