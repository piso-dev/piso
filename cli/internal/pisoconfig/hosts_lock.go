package pisoconfig

import (
	"fmt"
	"os"
	"time"
)

// A stable sidecar beside the hosts file is host-wide (not per PISO_DATA), and
// survives atomic hosts-file replacement. Never unlink it: another process may
// still hold its inode. The kernel releases the lock if a writer crashes.
func withHostsLock(path string, fn func() error) error {
	f, err := os.OpenFile(path+".piso.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		acquired, err := tryHostsLock(f)
		if err != nil {
			return err
		}
		if acquired {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for hosts lock %s", f.Name())
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer unlockHosts(f)
	return fn()
}

// ReconcileIngressHostsFrom serializes fetching the authoritative snapshot as
// well as reading/writing hosts, so an older fetch cannot overwrite a newer one.
// load must have a bounded deadline.
func ReconcileIngressHostsFrom(load func() ([]string, error)) error {
	return withHostsLock(HostsPath, func() error {
		names, err := load()
		if err != nil {
			return err
		}
		return reconcileHostsUnlocked(HostsPath, names)
	})
}
