//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

func processSignaledKill(ee *exec.ExitError) bool {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
		return ws.Signaled() && ws.Signal() == syscall.SIGKILL
	}
	return false
}
