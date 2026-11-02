package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"piso/cli/internal/pisoconfig"
)

func TestHostsSyncConvergesWithoutEventsAndWithStalledStream(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	old := pisoconfig.HostsPath
	path := filepath.Join(t.TempDir(), "hosts")
	pisoconfig.HostsPath = path
	defer func() { pisoconfig.HostsPath = old }()
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var live atomic.Bool
	var unavailable atomic.Bool
	var streamStarted atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/requests/stream" {
			streamStarted.Store(true)
			// No headers, no route events, no FIN. Reconciliation must continue.
			<-r.Context().Done()
			return
		}
		if unavailable.Load() {
			http.Error(w, "restarting", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if live.Load() {
			fmt.Fprint(w, `[{"name":"site","port":3000}]`)
		} else {
			fmt.Fprint(w, `[]`)
		}
	}))
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	successes := make(chan error, 100)
	go func() { done <- runHostsSync(ctx, ts.URL, hostsSyncInterval, func(err error) { successes <- err }) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("sync didn't stop/cancel its stalled stream")
		}
	}()
	wait := func(test func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !test() {
			if time.Now().After(deadline) {
				t.Fatal("sync didn't converge")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	contains := func(name string) bool { b, _ := os.ReadFile(path); return strings.Contains(string(b), name) }
	wait(func() bool { return contains("piso.local") && streamStarted.Load() })
	live.Store(true)
	wait(func() bool { return contains("site.piso.local") })
	// Errors preserve the last good hosts snapshot. Recovery needs no event.
	unavailable.Store(true)
	wait(func() bool {
		select {
		case err := <-successes:
			return err != nil
		default:
			return false
		}
	})
	if !contains("site.piso.local") {
		t.Fatal("failed fetch removed the last good snapshot")
	}
	live.Store(false)
	unavailable.Store(false)
	wait(func() bool { return !contains("site.piso.local") })
	if !contains("localhost") {
		t.Fatal("unrelated hosts entry lost")
	}
}

func TestHostsSyncRejectsUnauthorizedSnapshot(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	old := pisoconfig.HostsPath
	path := filepath.Join(t.TempDir(), "hosts")
	pisoconfig.HostsPath = path
	defer func() { pisoconfig.HostsPath = old }()
	original := "127.0.0.1 keep.piso.local # managed by piso\n"
	os.WriteFile(path, []byte(original), 0644)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unauthorized", 401) }))
	defer ts.Close()
	if err := syncIngressHostsAt(context.Background(), ts.URL); err == nil {
		t.Fatal("accepted 401")
	}
	b, _ := os.ReadFile(path)
	if string(b) != original {
		t.Fatal("error changed hosts file")
	}
}
