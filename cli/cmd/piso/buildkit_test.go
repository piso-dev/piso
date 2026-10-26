package main

import (
	"testing"
)

// A Docker CLI without the buildx plugin cannot build through compose v2, and
// the failure is fatal for the whole install. The Dockerfiles use no
// BuildKit-only syntax, so the legacy builder must be the fallback — never a
// hard "1" that blocks a host which merely lacks the plugin.
func TestBuildkitModeFallsBackWithoutBuildx(t *testing.T) {
	got := buildkitMode()
	if got != "0" && got != "1" {
		t.Fatalf("buildkitMode = %q, want \"0\" or \"1\"", got)
	}
	// Cached: repeated calls must not shell out again (and must agree).
	if again := buildkitMode(); again != got {
		t.Fatalf("buildkitMode not stable: %q then %q", got, again)
	}
}

// An explicit environment value must win, so a host can force either builder
// without editing code (and without waiting for the probe).
func TestBuildkitModeHonoursExplicitEnv(t *testing.T) {
	t.Setenv("DOCKER_BUILDKIT", "0")
	if got := buildkitMode(); got != "0" {
		t.Fatalf("buildkitMode = %q, want the explicit \"0\"", got)
	}
}
