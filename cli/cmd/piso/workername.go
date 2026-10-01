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
	workerNameOut io.Writer = os.Stderr
)

func openTTY() (*os.File, error) {
	if runtime.GOOS == "windows" {
		return os.OpenFile("CONIN$", os.O_RDWR, 0)
	}
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

func canPrompt() bool {
	if workerNameIn != os.Stdin {
		return true
	}
	f, err := openTTY()
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func promptWorkerName(proposed string) string {
	fmt.Fprintf(workerNameOut, "piso: worker piso-worker-%s does not exist yet\n", proposed)
	fmt.Fprintf(workerNameOut, "piso: worker name [%s]: ", proposed)
	r := workerNameIn
	if workerNameIn == os.Stdin {
		if f, err := openTTY(); err == nil {
			defer f.Close()
			r = f
		}
	}
	sc := bufio.NewScanner(r)
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
	return existingSlugFromContainers(dir, dockernet.ListNames("piso-worker-"), dockernet.ContainerWorkspace)
}

func existingSlugFromContainers(dir string, names []string, workspace func(string) (string, bool)) string {
	for _, name := range names {
		slug, ok := slugFromWorkerContainer(name)
		if !ok {
			continue
		}
		ws, ok := workspace(name)
		if !ok || !pathsEqual(ws, dir) {
			continue
		}
		return slug
	}
	return ""
}

func slugFromWorkerContainer(name string) (string, bool) {
	const prefix = "piso-worker-"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	slug := strings.TrimPrefix(name, prefix)
	if slug == "" || pisoconfig.ReservedSlug(slug) {
		return "", false
	}
	return slug, true
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
	existing := existingSlugForDir(dir)
	if existing != "" {
		if err := rejectNameFlag(nameFlag, existing); err != nil {
			return "", err
		}
		_ = pisoconfig.SaveProjectSlug(dir, existing)
		fmt.Fprintf(workerNameOut, "piso: worker piso-worker-%s already exists\n", existing)
		return existing, nil
	}

	saved, _ := pisoconfig.LoadProjectSlug(dir)
	proposed := saved
	if proposed == "" {
		proposed = pisoconfig.DefaultSlug(dir)
	}
	interactive := nameFlag == "" && canPrompt()
	typed := ""
	if interactive {
		typed = promptWorkerName(proposed)
	}
	slug, err := pisoconfig.DecideWorkerSlug(pisoconfig.NameDecision{
		Dir:         dir,
		NameFlag:    nameFlag,
		Proposed:    proposed,
		Interactive: interactive,
		Typed:       typed,
	})
	if err != nil {
		return "", err
	}
	if slugTaken(slug, dir) {
		return "", fmt.Errorf("worker name %q is already used by another project", slug)
	}
	if err := pisoconfig.SaveProjectSlug(dir, slug); err != nil {
		return "", err
	}
	return slug, nil
}

func rejectNameFlag(nameFlag, locked string) error {
	flag := pisoconfig.NormalizeWorkerName(nameFlag)
	if flag != "" && flag != locked {
		return fmt.Errorf("worker already named %q (--name is only when that worker does not exist)", locked)
	}
	return nil
}
