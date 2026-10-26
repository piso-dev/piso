package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"piso/cli/internal/pisoconfig"
)

func TestParseUpArgsNameAndDir(t *testing.T) {
	opts, err := parseUpArgs([]string{"--name", "myproj", "/tmp/work"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Name != "myproj" || opts.Dir != "/tmp/work" {
		t.Fatalf("got %+v", opts)
	}
	opts, err = parseUpArgs([]string{"--name=foo"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Name != "foo" || opts.Dir != "." {
		t.Fatalf("defaults: %+v", opts)
	}
}

func TestExistingSlugFromContainers(t *testing.T) {
	ws := map[string]string{
		"piso-worker-monitor": "/tmp/monitor",
		"piso-worker-demo":    "/tmp/proj",
	}
	lookup := func(name string) (string, bool) {
		p, ok := ws[name]
		return p, ok
	}
	got := existingSlugFromContainers("/tmp/proj", []string{"piso-worker-monitor", "piso-worker-demo"}, lookup)
	if got != "demo" {
		t.Fatalf("got %q, want demo", got)
	}
	if existingSlugFromContainers("/tmp/other", []string{"piso-worker-demo"}, lookup) != "" {
		t.Fatal("other dir must not match")
	}
}

func TestPromptWorkerName(t *testing.T) {
	var out bytes.Buffer
	oldIn, oldOut := workerNameIn, workerNameOut
	workerNameOut = &out
	defer func() {
		workerNameIn = oldIn
		workerNameOut = oldOut
	}()

	workerNameIn = strings.NewReader("\n")
	if got := promptWorkerName("from-git"); got != "from-git" {
		t.Fatalf("enter: %q", got)
	}
	workerNameIn = strings.NewReader("custom\n")
	if got := promptWorkerName("from-git"); got != "custom" {
		t.Fatalf("typed: %q", got)
	}
	if !strings.Contains(out.String(), "piso-worker-from-git") {
		t.Fatalf("prompt should show container name, got %q", out.String())
	}
}

// A cloud ship is named after the machine, not the directory it happens to be
// launched from: the same workspace mounts whatever repository a task asks for.
func TestChooseWorkerSlugUsesShipName(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	dir := t.TempDir()
	got, err := chooseWorkerSlug(dir, "", "ship-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ship-1" {
		t.Fatalf("slug = %q, want ship-1", got)
	}
	// The lock lives on the workspace root, so a checkout underneath resolves
	// back to the ship instead of to its own repository name.
	sub := filepath.Join(dir, "owner", "repo")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if slug, ok := pisoconfig.LoadProjectSlugUp(sub); !ok || slug != "ship-1" {
		t.Fatalf("LoadProjectSlugUp = %q %v, want ship-1", slug, ok)
	}
}

// No proposed name keeps the project behavior: the directory decides.
func TestChooseWorkerSlugFallsBackToDir(t *testing.T) {
	t.Setenv("PISO_DATA", t.TempDir())
	dir := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := chooseWorkerSlug(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "myproj" {
		t.Fatalf("slug = %q, want myproj", got)
	}
}

func TestShipSlugIsUsable(t *testing.T) {
	slug := shipSlug()
	if slug == "" {
		t.Skip("hostname sanitizes to nothing")
	}
	if slug != pisoconfig.SanitizeSlug(slug) || pisoconfig.ReservedSlug(slug) {
		t.Fatalf("shipSlug = %q is not a valid slug", slug)
	}
}
