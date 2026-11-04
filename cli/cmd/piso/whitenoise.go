package main

import "fmt"

// External state survives compose down --volumes; creation is idempotent and
// never removes or replaces existing cryptographic state. All placements call
// this from startGateway. Do not force-recreate the companion on gateway updates.
func startWhiteNoiseCompanion(file string, env map[string]string) error {
	return launchWhiteNoise(file, env, runEnv)
}

func launchWhiteNoise(file string, env map[string]string, run func(string, []string, map[string]string) error) error {
	if err := run("docker", []string{"volume", "create", "piso-whitenoise-state"}, env); err != nil {
		return fmt.Errorf("prepare connector state: %w", err)
	}
	if err := run("docker", []string{"compose", "-f", file, "-p", "piso", "up", "-d", "--build", "--no-deps", "whitenoise"}, env); err != nil {
		return fmt.Errorf("start connector: %w", err)
	}
	return nil
}
