// Package ownfile matches a written file's owner to its parent directory.
// The gateway runs as root and writes into a bind-mounted data directory;
// without this, identity.json and state.json are root-owned 0600 and the
// host CLI cannot read them.
package ownfile

import (
	"runtime"
)

// MatchDir chowns path to the uid/gid of its parent directory. It is a no-op
// on non-Linux hosts, when the parent is missing, and when the process lacks
// permission to chown (the host CLI writing its own files).
func MatchDir(path string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	return matchDir(path)
}
