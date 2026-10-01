package main

import (
	"bytes"
	"strings"
	"testing"
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
