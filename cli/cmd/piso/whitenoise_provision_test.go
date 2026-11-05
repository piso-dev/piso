package main

import (
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"piso/internal/nostrkey"
)

const provisionTestNpub = "npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptg"

func TestFinishGatewayLaunchOnlyExplicitStandalone(t *testing.T) {
	calls := 0
	failure := errors.New("test")
	provision := func() error { calls++; return failure }
	if err := finishGatewayLaunch(false, provision); err != nil || calls != 0 {
		t.Fatal(err, calls)
	}
	if err := finishGatewayLaunch(true, provision); !errors.Is(err, failure) || !strings.Contains(err.Error(), "gateway remains running") || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestStandaloneWhiteNoiseProvisioning(t *testing.T) {
	key, err := nostrkey.Normalize(provisionTestNpub)
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range []bool{false, true} {
		for _, image := range []bool{false, true} {
			var commands [][]string
			var events []string
			run := func(name string, args []string, env map[string]string) error {
				if name != "docker" || env["test"] != "env" {
					t.Fatal(name, env)
				}
				commands = append(commands, args)
				events = append(events, "docker")
				return nil
			}
			call := func(method, path string, body, out any) error {
				if path != "/api/v1/whitenoise" {
					t.Fatal(path)
				}
				events = append(events, method)
				if method == "GET" {
					rows := []whiteNoiseProvisionConnection{}
					if existing {
						rows = append(rows, whiteNoiseProvisionConnection{key, true})
					}
					raw, _ := json.Marshal(map[string]any{"connections": rows})
					return json.Unmarshal(raw, out)
				}
				if !reflect.DeepEqual(body, map[string]any{"operation": "connect", "publicKey": provisionTestNpub}) {
					t.Fatal(body)
				}
				return nil
			}
			bootstrap := func(expected string) ([]byte, error) {
				events = append(events, "bootstrap")
				if (existing && expected != key) || (!existing && expected != "") {
					t.Fatal(expected)
				}
				return json.Marshal(whiteNoiseBootstrap{key, provisionTestNpub, true})
			}
			if err := provisionWhiteNoiseWith("gateway.yaml", map[string]string{"test": "env"}, run, func(string) bool { return image }, call, bootstrap); err != nil {
				t.Fatal(err)
			}
			want := [][]string{}
			if !image {
				want = append(want, []string{"compose", "-f", "gateway.yaml", "-p", "piso", "build", "whitenoise"})
			}
			want = append(want, []string{"volume", "create", whiteNoiseVolume}, []string{"compose", "-f", "gateway.yaml", "-p", "piso", "up", "-d", "--no-build", "--no-deps", "whitenoise"})
			if !reflect.DeepEqual(commands, want) {
				t.Fatal(commands)
			}
			if events[0] != "GET" || events[len(events)-2] != "bootstrap" || events[len(events)-1] != "POST" {
				t.Fatal(events)
			}
		}
	}
}

func TestStandaloneWhiteNoiseFailsClosed(t *testing.T) {
	for _, stage := range []string{"ownership-unavailable", "other-owner", "invalid-account", "build", "volume", "start", "bootstrap", "invalid-bootstrap", "register"} {
		t.Run(stage, func(t *testing.T) {
			var events []string
			failure := errors.New("test failure")
			call := func(method, path string, body, out any) error {
				events = append(events, method)
				if method == "GET" {
					if stage == "ownership-unavailable" {
						return failure
					}
					rows := []whiteNoiseProvisionConnection{}
					if stage == "other-owner" {
						rows = append(rows, whiteNoiseProvisionConnection{strings.Repeat("0", 64), false})
					}
					if stage == "invalid-account" {
						rows = append(rows, whiteNoiseProvisionConnection{"broken", true})
					}
					raw, _ := json.Marshal(map[string]any{"connections": rows})
					return json.Unmarshal(raw, out)
				}
				return failure
			}
			run := func(_ string, args []string, _ map[string]string) error {
				which := "start"
				if args[0] == "volume" {
					which = "volume"
				} else if args[len(args)-2] == "build" {
					which = "build"
				}
				events = append(events, which)
				if stage == which {
					return failure
				}
				return nil
			}
			bootstrap := func(string) ([]byte, error) {
				events = append(events, "bootstrap")
				if stage == "bootstrap" {
					return nil, failure
				}
				if stage == "invalid-bootstrap" {
					return []byte(`{"npub":"nsec1secret"}`), nil
				}
				key, _ := nostrkey.Normalize(provisionTestNpub)
				return json.Marshal(whiteNoiseBootstrap{key, provisionTestNpub, true})
			}
			err := provisionWhiteNoiseWith("test", nil, run, func(string) bool { return false }, call, bootstrap)
			if err == nil {
				t.Fatal("unexpected success", events)
			}
			if stage != "register" && events[len(events)-1] == "POST" {
				t.Fatal("registered after failure", events)
			}
			if strings.HasPrefix(stage, "ownership") || stage == "other-owner" || stage == "invalid-account" {
				if !reflect.DeepEqual(events, []string{"GET"}) {
					t.Fatal(events)
				}
			}
		})
	}
}

// Exercise the actual container script with a fake supported bootstrap client;
// no Docker, keys or relays are needed for retry and lost-state coverage.
func TestWhiteNoiseBootstrapScriptRetainsIdentityBeforePublication(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	key, err := nostrkey.Normalize(provisionTestNpub)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	script := strings.ReplaceAll(whiteNoiseBootstrapScript, "/data/", home+"/")
	encoded, _ := json.Marshal(script)
	prelude := `import json, os, subprocess, sys, types
script = ` + string(encoded) + `
key = "` + key + `"
npub = "` + provisionTestNpub + `"
path = "` + home + `/piso-bootstrap.json"
mode = sys.argv[2]
count = 0
def run(args, **kwargs):
    global count
    count += 1
    assert "--invite-policy" in args and args[args.index("--invite-policy")+1] == "allowlist"
    if mode != "first":
        assert "--no-create" in args
        assert args[args.index("--account-id-hex")+1] == key
    if count == 1:
        assert "--no-publish-key-package" in args
    else:
        assert "--no-publish-key-package" not in args
        assert json.load(open(path))["account_id_hex"] == key
    if mode == "lost" or (mode == "first" and count == 2):
        return types.SimpleNamespace(returncode=1, stdout=b"", stderr=b"private error")
    return types.SimpleNamespace(returncode=0, stdout=json.dumps({"account_id_hex":key,"npub":npub,"local_signing":True}).encode())
subprocess.run = run
exec(script)
`
	// First publication fails, but the public marker must already be durable.
	if raw, err := exec.Command(python, "-c", prelude, "", "first").CombinedOutput(); err == nil || !strings.Contains(string(raw), "identity retained") {
		t.Fatalf("first attempt: %s %v", raw, err)
	}
	// A retry selects the saved key with --no-create even without registration.
	if raw, err := exec.Command(python, "-c", prelude, "", "retry").CombinedOutput(); err != nil {
		t.Fatalf("retry: %s %v", raw, err)
	}
	// Missing local keys cannot trigger a replacement.
	if raw, err := exec.Command(python, "-c", prelude, "", "lost").CombinedOutput(); err == nil || strings.Contains(string(raw), "private error") {
		t.Fatalf("lost state: %s %v", raw, err)
	}
	// A conflicting coordinator identity fails before contacting the connector.
	if raw, err := exec.Command(python, "-c", prelude, strings.Repeat("f", 64), "retry").CombinedOutput(); err == nil || !strings.Contains(string(raw), "does not match") {
		t.Fatalf("mismatch: %s %v", raw, err)
	}
}

func TestBootstrapRejectsReplacementAndInvalidResponses(t *testing.T) {
	key, _ := nostrkey.Normalize(provisionTestNpub)
	for _, result := range []whiteNoiseBootstrap{
		{key, provisionTestNpub, false},
		{strings.Repeat("f", 64), provisionTestNpub, true},
		{key, "nsec1secret", true},
	} {
		raw, _ := json.Marshal(result)
		if _, err := validateWhiteNoiseBootstrap(raw, ""); err == nil {
			t.Fatal(result)
		}
	}
	raw, _ := json.Marshal(whiteNoiseBootstrap{key, provisionTestNpub, true})
	if _, err := validateWhiteNoiseBootstrap(raw, strings.Repeat("f", 64)); err == nil {
		t.Fatal("replacement allowed")
	}
}
