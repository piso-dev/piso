package main

import (
	"errors"
	"fmt"

	"piso/cli/internal/dockernet"
	"piso/cli/internal/pisoconfig"
)

// The connector is a side service, not part of the gateway's job: `piso up`
// never touches it, and no gateway start builds it. A Rust release build of
// wn-agent is minutes of work on a gateway host and pointless on a laptop,
// where White Noise is not used at all. The image is built once, explicitly,
// by `piso whitenoise build`.
const (
	whiteNoiseImage     = "piso-whitenoise:04e94720d6ea5dcf93daa3ccccf10763d10bb2c5"
	whiteNoiseContainer = "piso-whitenoise"
	whiteNoiseVolume    = "piso-whitenoise-state"
)

// errWhiteNoiseNotBuilt is the normal state of a host that never asked for
// White Noise. Gateway startup treats it as silence, not a warning.
var errWhiteNoiseNotBuilt = errors.New("White Noise connector image is not built (run `piso whitenoise build`)")

// whiteNoiseImageExists reports whether an image is already on this host.
// Compose builds a service whose image is missing, so this check is what keeps
// a gateway start from spending minutes on Rust compilation.
func whiteNoiseImageExists(image string) bool {
	_, err := dockerOutputLite("image", "inspect", "--format", "{{.Id}}", image)
	return err == nil
}

func cmdWhiteNoise(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: piso whitenoise build|start|status")
	}
	switch args[0] {
	case "build":
		return cmdWhiteNoiseBuild()
	case "start":
		return cmdWhiteNoiseStart()
	case "status":
		return cmdWhiteNoiseStatus()
	default:
		return errors.New("usage: piso whitenoise build|start|status")
	}
}

// cmdWhiteNoiseBuild is the only path that may compile the connector. It is
// deliberately not wired into `piso up` or any gateway start.
func cmdWhiteNoiseBuild() error {
	if err := dockernet.AssertLinuxEngine(); err != nil {
		return err
	}
	file, env, err := whiteNoiseCompose()
	if err != nil {
		return err
	}
	fmt.Printf("piso: building %s (pinned MDK source; the first build compiles wn-agent with Rust and takes several minutes)\n", whiteNoiseImage)
	if err := runEnv("docker", []string{"compose", "-f", file, "-p", "piso", "build", "whitenoise"}, env); err != nil {
		return fmt.Errorf("build connector image: %w", err)
	}
	fmt.Println("piso: connector image built; `piso whitenoise start` runs it")
	return nil
}

// cmdWhiteNoiseStart starts the companion on an existing image. It never
// builds: gateway start paths use the same launch and stay build-free.
func cmdWhiteNoiseStart() error {
	if err := dockernet.AssertLinuxEngine(); err != nil {
		return err
	}
	file, env, err := whiteNoiseCompose()
	if err != nil {
		return err
	}
	if err := launchWhiteNoise(file, env, runEnv, whiteNoiseImageExists); err != nil {
		return err
	}
	fmt.Printf("piso: %s started\n", whiteNoiseContainer)
	return nil
}

func cmdWhiteNoiseStatus() error {
	image := "not built (run `piso whitenoise build`)"
	if whiteNoiseImageExists(whiteNoiseImage) {
		image = "built"
	}
	container := "absent"
	switch {
	case containerRunning(whiteNoiseContainer):
		container = "running"
	case whiteNoiseContainerExists():
		container = "stopped"
	}
	fmt.Printf("image      %s: %s\n", whiteNoiseImage, image)
	fmt.Printf("container  %s: %s\n", whiteNoiseContainer, container)
	fmt.Println("note: `piso up` does not start White Noise; only `piso whitenoise start`, `piso setup --rebuild` and `piso gateway launch` do")
	return nil
}

func whiteNoiseContainerExists() bool {
	id, err := dockerOutputLite("ps", "-aq", "--filter", "name=^"+whiteNoiseContainer+"$")
	return err == nil && id != ""
}

func whiteNoiseCompose() (string, map[string]string, error) {
	file, err := pisoconfig.GatewayCompose()
	if err != nil {
		return "", nil, err
	}
	env, err := dockerComposeEnv()
	if err != nil {
		return "", nil, err
	}
	return file, env, nil
}

// startWhiteNoiseCompanion is the best-effort start every gateway path except
// `piso up` uses. A missing image is not an error worth reporting: it is the
// state of every host that has not opted into White Noise.
func startWhiteNoiseCompanion(file string, env map[string]string) error {
	return launchWhiteNoise(file, env, runEnv, whiteNoiseImageExists)
}

// launchWhiteNoise starts the connector from its pinned image, creating the
// external state volume first (idempotent; it never removes cryptographic
// state). It never passes --build: an image that has run before is reused as
// is, and an image that is missing is reported instead of compiled during a
// gateway start.
func launchWhiteNoise(file string, env map[string]string, run func(string, []string, map[string]string) error, imageExists func(string) bool) error {
	if !imageExists(whiteNoiseImage) {
		return errWhiteNoiseNotBuilt
	}
	if err := run("docker", []string{"volume", "create", whiteNoiseVolume}, env); err != nil {
		return fmt.Errorf("prepare connector state: %w", err)
	}
	if err := run("docker", []string{"compose", "-f", file, "-p", "piso", "up", "-d", "--no-deps", "whitenoise"}, env); err != nil {
		return fmt.Errorf("start connector: %w", err)
	}
	return nil
}
