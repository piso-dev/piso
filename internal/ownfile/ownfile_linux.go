package ownfile

import (
	"os"
	"path/filepath"
	"syscall"
)

func matchDir(path string) error {
	dir := filepath.Dir(path)
	st, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := os.Chown(path, int(sys.Uid), int(sys.Gid)); err != nil {
		// Not root, or the filesystem forbids chown. The writer already
		// owns the file, which is the right outcome for the host CLI.
		return nil
	}
	return nil
}
