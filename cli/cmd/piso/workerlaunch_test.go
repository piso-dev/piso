package main

import (
	"strings"
	"testing"
)

func TestWorkerLaunchRequiresExplicitServer(t *testing.T) {
	t.Setenv("PISO_SERVER_URL", "https://ignored.example.com")
	t.Setenv("PISO_DATA", t.TempDir())
	for _, args := range [][]string{nil, {"--server", ""}, {"--server", "   "}} {
		if err := cmdWorkerLaunch(args); err == nil || !strings.Contains(err.Error(), "--server is required") {
			t.Fatalf("args %v: expected required-server error, got %v", args, err)
		}
	}
}
