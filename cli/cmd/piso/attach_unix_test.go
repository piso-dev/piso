//go:build unix

package main

import (
	"os/exec"
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
