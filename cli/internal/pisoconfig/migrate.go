package pisoconfig

import (
	"fmt"
	"os"
	"path/filepath"
)

// LegacyDataFiles are host-side gateway files that older piso versions stored
// next to the repo (./.piso) instead of ~/.piso.
var LegacyDataFiles = []string{
	"state.json",
	"patterns.json",
	"requests.jsonl",
	"ca.crt",
	"ca.key",
	"ports.json",
}

// ImportLegacyData copies files from srcDir into dstDir when the destination
// is missing them. Existing dest files are left alone so a live secrets
// database is never replaced by a stale checkout copy.
func ImportLegacyData(srcDir, dstDir string) ([]string, error) {
	src, err := filepath.Abs(srcDir)
	if err != nil {
		return nil, fmt.Errorf("migrate source: %w", err)
	}
	dst, err := filepath.Abs(dstDir)
	if err != nil {
		return nil, fmt.Errorf("migrate dest: %w", err)
	}
	if src == dst {
		return nil, nil
	}

	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("migrate source %s: %w", src, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("migrate source %s is not a directory", src)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return nil, err
	}

	var copied []string
	for _, name := range LegacyDataFiles {
		from := filepath.Join(src, name)
		to := filepath.Join(dst, name)
		if _, err := os.Stat(from); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return copied, fmt.Errorf("read %s: %w", from, err)
		}
		if _, err := os.Stat(to); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return copied, fmt.Errorf("stat %s: %w", to, err)
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return copied, fmt.Errorf("read %s: %w", from, err)
		}
		if err := os.WriteFile(to, data, 0o600); err != nil {
			return copied, fmt.Errorf("write %s: %w", to, err)
		}
		copied = append(copied, name)
	}
	return copied, nil
}
