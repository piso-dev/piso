package main

import (
	"errors"
	"reflect"
	"testing"
)

// The connector image carries a Rust build of upstream Marmot. A start must
// reuse it, never rebuild it, and never compile one for a host that has not
// asked for White Noise.
func TestWhiteNoiseLaunchReusesExistingImageWithoutBuild(t *testing.T) {
	var calls [][]string
	run := func(name string, args []string, env map[string]string) error {
		if name != "docker" || !reflect.DeepEqual(env, map[string]string{"PISO_DATA": "test"}) {
			t.Fatal(name, env)
		}
		calls = append(calls, args)
		return nil
	}
	exists := func(image string) bool {
		if image != whiteNoiseImage {
			t.Fatalf("checked %q, want %q", image, whiteNoiseImage)
		}
		return true
	}
	if err := launchWhiteNoise("gateway.yaml", map[string]string{"PISO_DATA": "test"}, run, exists); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"volume", "create", whiteNoiseVolume},
		{"compose", "-f", "gateway.yaml", "-p", "piso", "up", "-d", "--no-deps", "whitenoise"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("%v", calls)
	}
}

func TestWhiteNoiseLaunchSkipsMissingImageInsteadOfBuilding(t *testing.T) {
	var calls int
	run := func(string, []string, map[string]string) error { calls++; return nil }
	err := launchWhiteNoise("gateway.yaml", nil, run, func(string) bool { return false })
	if !errors.Is(err, errWhiteNoiseNotBuilt) {
		t.Fatalf("err = %v, want errWhiteNoiseNotBuilt", err)
	}
	if calls != 0 {
		t.Fatalf("ran %d docker commands for a missing image", calls)
	}
}

func TestWhiteNoiseLaunchStopsOnStateFailure(t *testing.T) {
	calls := 0
	err := launchWhiteNoise("gateway.yaml", nil, func(string, []string, map[string]string) error { calls++; return errors.New("unavailable") }, func(string) bool { return true })
	if err == nil || calls != 1 {
		t.Fatal(calls, err)
	}
}
