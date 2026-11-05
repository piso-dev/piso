package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"piso/internal/nostrkey"
)

// Only an explicit `gateway launch --standalone` calls this completion hook.
// Enrollment has already succeeded; a provisioning failure must not undo it.
func finishGatewayLaunch(enabled bool, provision func() error) error {
	if !enabled {
		return nil
	}
	if err := provision(); err != nil {
		return fmt.Errorf("gateway remains running; White Noise setup incomplete (rerun `piso gateway launch --standalone`): %w", err)
	}
	return nil
}

type whiteNoiseProvisionConnection struct {
	AccountID  string `json:"accountId"`
	Manageable bool   `json:"manageable"`
}

type whiteNoiseBootstrap struct {
	AccountID    string `json:"account_id_hex"`
	Npub         string `json:"npub"`
	LocalSigning bool   `json:"local_signing"`
}

// Fetch shared ownership before creating anything. A coordinator outage is not
// permission to create a second identity, and a registered identity must exist
// locally: bootstrap uses --no-create in that case.
func whiteNoiseProvisionAccount(rows []whiteNoiseProvisionConnection) (string, error) {
	if len(rows) == 0 {
		return "", nil
	}
	if len(rows) != 1 || !rows[0].Manageable {
		return "", errors.New("White Noise identity belongs to another gateway; automatic handoff is not supported")
	}
	key, err := nostrkey.Normalize(rows[0].AccountID)
	if err != nil || key != rows[0].AccountID {
		return "", errors.New("invalid registered White Noise identity")
	}
	return key, nil
}

func validateWhiteNoiseBootstrap(raw []byte, expected string) (whiteNoiseBootstrap, error) {
	var result whiteNoiseBootstrap
	if json.Unmarshal(raw, &result) != nil || !result.LocalSigning || !strings.HasPrefix(result.Npub, "npub1") {
		return result, errors.New("invalid connector bootstrap response")
	}
	key, err := nostrkey.Normalize(result.Npub)
	if err != nil || key != result.AccountID || (expected != "" && key != expected) {
		return result, errors.New("connector bootstrap identity does not match")
	}
	return result, nil
}

func provisionStandaloneWhiteNoise() error {
	file, env, err := whiteNoiseCompose()
	if err != nil {
		return err
	}
	return provisionWhiteNoiseWith(file, env, runEnv, whiteNoiseImageExists, gatewayAuthCall, bootstrapWhiteNoiseContainer)
}

func provisionWhiteNoiseWith(file string, env map[string]string,
	run func(string, []string, map[string]string) error, exists func(string) bool,
	call func(string, string, any, any) error, bootstrap func(string) ([]byte, error)) error {
	var shared struct {
		Connections []whiteNoiseProvisionConnection `json:"connections"`
	}
	if err := call("GET", "/api/v1/whitenoise", nil, &shared); err != nil {
		return fmt.Errorf("check White Noise ownership: %w", err)
	}
	if shared.Connections == nil {
		return errors.New("invalid White Noise ownership response; refusing to provision")
	}
	expected, err := whiteNoiseProvisionAccount(shared.Connections)
	if err != nil {
		return err
	}
	if !exists(whiteNoiseImage) {
		fmt.Println("piso: building White Noise connector (first build may take several minutes)")
		if err := run("docker", []string{"compose", "-f", file, "-p", "piso", "build", "whitenoise"}, env); err != nil {
			return fmt.Errorf("build White Noise connector: %w", err)
		}
	}
	if err := launchWhiteNoise(file, env, run, func(string) bool { return true }); err != nil {
		return err
	}
	fmt.Println("piso: provisioning White Noise identity")
	raw, err := bootstrap(expected)
	if err != nil {
		return err
	}
	result, err := validateWhiteNoiseBootstrap(raw, expected)
	if err != nil {
		return err
	}
	if err := call("POST", "/api/v1/whitenoise", map[string]any{"operation": "connect", "publicKey": result.Npub}, nil); err != nil {
		return fmt.Errorf("register White Noise identity (local identity retained): %w", err)
	}
	fmt.Printf("piso: White Noise agent %s\n", result.Npub)
	fmt.Println("piso: Access → White Noise → Link person with your public npub, then invite this agent from your White Noise app")
	return nil
}

// The client talks to the existing daemon; it never starts a second writer.
// Persist a public identity marker atomically in the connector home before
// registration. Retries after a failed registration reuse that account. If
// its keys are later lost, --no-create prevents silent replacement.
const whiteNoiseBootstrapScript = `import fcntl, json, os, subprocess, sys
os.umask(0o077)
lock = open("/data/.piso-bootstrap.lock", "a")
try:
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
except BlockingIOError:
    sys.exit("another White Noise provisioning operation is running")
path = "/data/piso-bootstrap.json"
expected = sys.argv[1]
if os.path.exists(path):
    with open(path) as f:
        saved = json.load(f)["account_id_hex"]
    if expected and saved != expected:
        sys.exit("stored identity does not match registered identity")
    expected = saved
args = ["wn-agent", "bootstrap", "--json", "--home", "/data",
        "--socket", "/run/whitenoise/agent.sock", "--label", "piso-agent",
        "--invite-policy", "allowlist", "--no-quic",
        "--wait-for-socket", "60", "--request-timeout", "30"]
if expected:
    args += ["--account-id-hex", expected, "--no-create"]
# Persist the identity before publication, so a relay failure cannot leave an
# untracked identity that a later retry might silently replace.
result = subprocess.run(args + ["--no-publish-key-package"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=150)
if result.returncode:
    sys.exit("connector bootstrap failed; check connector health and relay connectivity")
out = json.loads(result.stdout)
key = out["account_id_hex"]
if not out.get("local_signing") or len(key) != 64 or any(c not in "0123456789abcdef" for c in key) or (expected and key != expected):
    sys.exit("invalid bootstrap identity")
with open(path + ".tmp", "w") as f:
    json.dump({"account_id_hex": key}, f)
    f.flush()
    os.fsync(f.fileno())
os.replace(path + ".tmp", path)
# Apply the policy before publication even when reusing an existing account.
args = ["wn-agent", "bootstrap", "--json", "--home", "/data",
        "--socket", "/run/whitenoise/agent.sock", "--label", "piso-agent",
        "--account-id-hex", key, "--no-create", "--invite-policy", "allowlist",
        "--no-quic", "--wait-for-socket", "60", "--request-timeout", "30"]
result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=150)
if result.returncode:
    sys.exit("identity retained; KeyPackage publication failed; check relay connectivity")
print(result.stdout.decode(), end="")
`

func bootstrapWhiteNoiseContainer(expected string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "docker", "exec", whiteNoiseContainer, "python3", "-c", whiteNoiseBootstrapScript, expected).Output()
	if err != nil {
		return nil, errors.New("White Noise bootstrap failed or timed out; identity state retained; check connector health and relay connectivity")
	}
	return raw, nil
}
