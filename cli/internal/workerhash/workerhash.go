// Package workerhash fingerprints the worker Docker build context so
// `piso up` can skip --build when image files are unchanged.
package workerhash

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

// Files are hashed in this order. Adding a file here is a rebuild.
// package.json is the host pi extension list staged into the build context.
var Files = []string{"Dockerfile", "entrypoint.sh", "planning-watch.sh", "loopback-forward.py", "context-watch.sh", "ports-watch.sh", "activity-watch.sh", "informant-help.sh", "INFORMANT.md", "MONITOR.md", "monitor-loop.sh", "package.json"}

// ContextHash returns a short hex digest of the worker build files.
func ContextHash(workerDir string) (string, error) {
	h := sha256.New()
	for _, name := range Files {
		f, err := os.Open(filepath.Join(workerDir, name))
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		h.Write([]byte{0})
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if len(sum) > 16 {
		return sum[:16], nil
	}
	return sum, nil
}
