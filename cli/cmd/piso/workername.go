package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"piso/cli/internal/dockernet"
	"piso/cli/internal/pisoconfig"
)

var (
	workerNameIn  io.Reader = os.Stdin
	workerNameOut io.Writer = os.Stdout
)

func stdinIsTTY() bool {
	f, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return f.Mode()&os.ModeCharDevice != 0
}

func promptWorkerName(proposed string) string {
	fmt.Fprintf(workerNameOut, "piso: worker piso-worker-%s does not exist yet\n", proposed)
	fmt.Fprintf(workerNameOut, "piso: worker name [%s]: ", proposed)
	sc := bufio.NewScanner(workerNameIn)
	if !sc.Scan() {
		return proposed
	}
	line := strings.TrimSpace(sc.Text())
	if line == "" {
		return proposed
	}
	return line
}

func existingSlugForDir(dir string) string {
	if slug, ok := pisoconfig.ComposeSlugForDir(dir); ok {
		return slug
	}
	legacy := pisoconfig.SlugFromDir(dir)
	ws, ok := dockernet.ContainerWorkspace("piso-worker-" + legacy)
	if !ok {
		return ""
	}
	if !pathsEqual(ws, dir) {
		return ""
	}
	return legacy
}

func pathsEqual(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if abs, err := filepath.Abs(a); err == nil {
		a = abs
	}
	if abs, err := filepath.Abs(b); err == nil {
		b = abs
	}
	if a == b {
		return true
	}
	return runtime.GOOS == "windows" && strings.EqualFold(a, b)
}

func slugTaken(slug, dir string) bool {
	if pisoconfig.SlugTakenByOther(slug, dir) {
		return true
	}
	ws, ok := dockernet.ContainerWorkspace("piso-worker-" + slug)
	if !ok {
		return false
	}
	return !pathsEqual(ws, dir)
}

func chooseWorkerSlug(dir, nameFlag string) (string, error) {
	saved, _ := pisoconfig.LoadProjectSlug(dir)
	existing := ""
	if saved == "" {
		existing = existingSlugForDir(dir)
	}
	proposed := ""
	if saved == "" && existing == "" {
		proposed = pisoconfig.DefaultSlug(dir)
	}
	interactive := nameFlag == "" && saved == "" && existing == "" && stdinIsTTY()
	typed := ""
	if interactive {
		typed = promptWorkerName(proposed)
	}
	slug, err := pisoconfig.DecideWorkerSlug(pisoconfig.NameDecision{
		Dir:         dir,
		NameFlag:    nameFlag,
		Saved:       saved,
		Existing:    existing,
		Proposed:    proposed,
		Interactive: interactive,
		Typed:       typed,
	})
	if err != nil {
		return "", err
	}
	if saved == "" && existing == "" && slugTaken(slug, dir) {
		return "", fmt.Errorf("worker name %q is already used by another project", slug)
	}
	if err := pisoconfig.SaveProjectSlug(dir, slug); err != nil {
		return "", err
	}
	return slug, nil
}
