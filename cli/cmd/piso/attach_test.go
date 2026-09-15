package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsKilled137(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if !isKilled137(err) {
		t.Fatalf("isKilled137(%v) = false, want true", err)
	}
	got := attachExecErr(err)
	if got == nil || !strings.Contains(got.Error(), "SIGKILL") {
		t.Fatalf("attachExecErr(%v) = %v", err, got)
	}
}

func TestAttachExecErrPassThrough(t *testing.T) {
	if attachExecErr(nil) != nil {
		t.Fatal("nil should pass through")
	}
	err := errors.New("exit status 1")
	if attachExecErr(err) != err {
		t.Fatalf("non-137 should pass through, got %v", attachExecErr(err))
	}
	if isKilled137(fmt.Errorf("exit status 137")) {
		t.Fatal("plain error must not match; need exec.ExitError")
	}
}

func TestWorkerSeedFiles(t *testing.T) {
	root := repoRoot(t)
	ep, err := os.ReadFile(filepath.Join(root, "worker", "entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ep), "/run/piso-ready") {
		t.Fatal("entrypoint must write /run/piso-ready before exec")
	}
	tmpl, err := os.ReadFile(filepath.Join(root, "compose", "worker.yaml.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"TMPDIR: /root/.pi/agent/tmp",
		"NODE_COMPILE_CACHE: /root/.pi/agent/.node-compile-cache",
	} {
		if !strings.Contains(string(tmpl), want) {
			t.Fatalf("worker compose missing %q", want)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}
