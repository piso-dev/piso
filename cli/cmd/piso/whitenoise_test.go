package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestWhiteNoiseLaunchPreservesStateAndIndependentLifecycle(t *testing.T) {
	var calls [][]string
	run := func(name string, args []string, env map[string]string) error {
		if name != "docker" || !reflect.DeepEqual(env, map[string]string{"PISO_DATA": "test"}) {
			t.Fatal(name, env)
		}
		calls = append(calls, args)
		return nil
	}
	if err := launchWhiteNoise("gateway.yaml", map[string]string{"PISO_DATA": "test"}, run); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"volume", "create", "piso-whitenoise-state"}, {"compose", "-f", "gateway.yaml", "-p", "piso", "up", "-d", "--build", "--no-deps", "whitenoise"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("%v", calls)
	}
}
func TestWhiteNoiseLaunchStopsOnStateFailure(t *testing.T) {
	calls := 0
	err := launchWhiteNoise("gateway.yaml", nil, func(string, []string, map[string]string) error { calls++; return errors.New("unavailable") })
	if err == nil || calls != 1 {
		t.Fatal(calls, err)
	}
}
