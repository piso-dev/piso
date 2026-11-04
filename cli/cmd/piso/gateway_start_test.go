package main

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestLaunchGatewayCoreBuildsOnlyGatewayBeforeBuildDisabledStartup(t *testing.T) {
	for _, monitor := range []bool{false, true} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("monitor=%t/force=%t", monitor, force), func(t *testing.T) {
				env := map[string]string{"PISO_DATA": "test"}
				var calls [][]string
				run := func(name string, args []string, gotEnv map[string]string) error {
					if name != "docker" || !reflect.DeepEqual(gotEnv, env) {
						t.Fatalf("unexpected command/environment: %s %v", name, gotEnv)
					}
					calls = append(calls, args)
					return nil
				}
				if err := launchGatewayCore("gateway.yaml", env, force, monitor, run); err != nil {
					t.Fatal(err)
				}
				up := []string{"compose", "-f", "gateway.yaml", "-p", "piso", "up", "-d", "--no-build", "-t", "0"}
				if force {
					up = append(up, "--force-recreate")
				}
				up = append(up, "gateway")
				if monitor {
					up = append(up, "monitor")
				}
				want := [][]string{
					{"volume", "create", whiteNoiseVolume},
					{"compose", "-f", "gateway.yaml", "-p", "piso", "build", "gateway"},
					up,
				}
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("commands = %v, want %v", calls, want)
				}
			})
		}
	}
}

func TestLaunchGatewayCoreStopsOnFailure(t *testing.T) {
	for failAt, message := range []string{"prepare gateway project volumes", "gateway build", "gateway up"} {
		t.Run(message, func(t *testing.T) {
			failure := errors.New("test failure")
			calls := 0
			run := func(string, []string, map[string]string) error {
				calls++
				if calls == failAt+1 {
					return failure
				}
				return nil
			}
			err := launchGatewayCore("gateway.yaml", nil, false, false, run)
			if !errors.Is(err, failure) || !strings.Contains(err.Error(), message) {
				t.Fatalf("error = %v, want wrapped failure with %q", err, message)
			}
			if calls != failAt+1 {
				t.Fatalf("ran %d commands, want %d", calls, failAt+1)
			}
		})
	}
}
