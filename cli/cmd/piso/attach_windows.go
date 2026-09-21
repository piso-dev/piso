//go:build windows

package main

import "os/exec"

func processSignaledKill(ee *exec.ExitError) bool {
	// docker exec maps a Linux SIGKILL to exit 137; that is handled in
	// isKilled137. A local Windows process does not report SIGKILL.
	return false
}
