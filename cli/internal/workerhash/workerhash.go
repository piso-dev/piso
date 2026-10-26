// Package workerhash fingerprints the worker Docker build context so
// `piso up` can skip --build when image files are unchanged.
package workerhash

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Files are hashed in this order. Adding a file here is a rebuild.
// package.json is the host pi extension list staged into the build context.
// The relay sources are not in this list: CopyWorkerSkeleton stages them as
// trees and ContextHash walks those via ExtraTrees.
var Files = []string{"Dockerfile", "entrypoint.sh", "piso-models.ts", "project-footer.ts", "planning-watch.sh", "loopback-forward.py", "context-watch.sh", "ports-watch.sh", "activity-watch.sh", "activity-state.ts", "activity-state.py", "mcp-add.sh", "workbench.sh", "mcp-merge.py", "checkin-watch.sh", "egress-forward.py", "gateway-post.sh", "WORKLOOP.md", "INFORMANT.md", "CAPABILITIES.md", "MONITOR.md", "TASKLOOP.md", "task-pick.py", "task-watch.sh", "supervisor-session.py", "monitor-loop.sh", "package.json"}

// ExtraTrees are directories staged next to Files. A change anywhere under
// them (sorted walk) changes the worker image hash.
var ExtraTrees = []string{"relay-src"}

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
	for _, name := range ExtraTrees {
		if err := hashTree(h, filepath.Join(workerDir, name)); err != nil {
			return "", err
		}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if len(sum) > 16 {
		return sum[:16], nil
	}
	return sum, nil
}

// hashTree mixes every regular file under root, in sorted relative-path
// order, so a relay source edit changes the worker image hash.
func hashTree(h io.Writer, root string) error {
	var paths []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, rel := range paths {
		if _, err := io.WriteString(h, rel); err != nil {
			return err
		}
		h.Write([]byte{0})
		f, err := os.Open(filepath.Join(root, rel))
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		h.Write([]byte{0})
	}
	return nil
}
