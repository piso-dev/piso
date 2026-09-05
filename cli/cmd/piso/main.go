// Command piso is the piso CLI: `piso up` starts gateway + worker for the
// current directory, `piso attach` enters the worker running pi, and the rest
// control the gateway (secrets, expose, logs, dashboard).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"piso/cli/internal/dockernet"
	"piso/cli/internal/piprofile"
	"piso/cli/internal/pisoconfig"
	"piso/cli/internal/pkgstamp"
	"piso/cli/internal/workerhash"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "up":
		err = cmdUp(args)
	case "down":
		err = cmdDown(args)
	case "attach":
		err = cmdAttach(args)
	case "status":
		err = cmdStatus(args)
	case "secrets":
		err = cmdSecrets(args)
	case "expose":
		err = cmdExpose(args)
	case "logs":
		err = cmdLogs(args)
	case "dashboard":
		err = cmdDashboard(args)
	case "setup":
		err = cmdSetup(args)
	case "version":
		fmt.Println("piso", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "piso:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`piso — isolate an AI agent in a Docker worker behind a MITM gateway

Usage:
  piso up [dir] [--proxy-port N] [--ctrl-port N] [--ingress-port N]
                         ensure gateway + worker for [dir] (default: cwd)
  piso down              stop this project's worker (gateway stays up)
  piso attach            enter the worker and run pi (new session if none exist)
  piso status            show gateway + worker state
  piso secrets list|add|rm   manage gateway secrets (real values never leave it)
  piso expose <port> [--name n]  reverse-proxy a worker port as https://n.piso.local
  piso logs [--follow]   tail the gateway request log (SSE when --follow)
  piso dashboard         open the gateway web UI (http://piso.local)
  piso setup [--rebuild] import leftover data and (with --rebuild) recreate the gateway

Host ports default to 8080 (proxy), 80 (control / http://piso.local), 8082 (ingress).
If a port is taken, piso up exits with the flag to override
(--proxy-port, --ctrl-port, --ingress-port). Env: PISO_*_PORT.
make install runs piso setup --rebuild so an existing install is replaced
in place (binaries, share tree, gateway image, and state.json schema).
`)
}

// ---- up / down ----

func cmdUp(args []string) error {
	dir, cliPorts, err := parseUpArgs(args)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	proj, err := pisoconfig.Resolve(abs)
	if err != nil {
		return err
	}
	if home, err := pisoconfig.Home(); err == nil {
		fmt.Printf("piso: using %s\n", home)
	}
	fmt.Printf("piso: project %q (slug %s)\n", proj.Dir, proj.Slug)

	ports, err := pisoconfig.ResolveHostPorts(cliPorts)
	if err != nil {
		return err
	}
	if err := pisoconfig.EnsurePisoLocal(); err != nil {
		return err
	}
	if err := pisoconfig.EnsureWorkerPlaceholdersEnv(proj.Slug); err != nil {
		return err
	}
	if err := pisoconfig.EnsurePiProfileFiles(); err != nil {
		return err
	}
	// If our control plane is already healthy, the host ports are ours.
	// Otherwise fail before compose if something else owns them.
	if !healthy(pisoconfig.ControlAPIURL()) {
		if err := pisoconfig.CheckHostPortsFree(ports); err != nil {
			return err
		}
	}

	// 1. gateway (shared, one per machine). Compose will not flip Internal on
	// an already-created piso_vpc, so tear down a leaky one first.
	if err := startGateway(false); err != nil {
		return err
	}
	if err := waitGatewayHealthy(); err != nil {
		return err
	}
	syncIngressHosts()
	if err := syncPiProfile(); err != nil {
		return err
	}
	// 2. worker (per-project). Stage host extensions into the image build
	// context, then rebuild only when that context hash changes.
	if n, err := prepareWorkerBuild(); err != nil {
		return err
	} else if n > 0 {
		fmt.Printf("piso: baking %d host extensions into the worker image\n", n)
	}
	composeEnv, err := dockerComposeEnv()
	if err != nil {
		return err
	}
	workerFile, err := pisoconfig.WriteWorkerCompose(proj)
	if err != nil {
		return err
	}
	upArgs := []string{"compose", "-f", workerFile, "-p", "piso-" + proj.Slug, "up", "-d", "-t", "0"}
	if workerNeedsBuild() {
		upArgs = append(upArgs, "--build")
	}
	if err := runEnv("docker", upArgs, composeEnv); err != nil {
		return fmt.Errorf("worker up: %w", err)
	}
	// Register the worker's slug↔IP identity with the gateway so request logs
	// can be tagged with the project slug. Refreshed on every `piso up`.
	_ = registerWorkerWithGateway(proj)
	fmt.Printf("piso: worker %s ready. Run `piso attach`.\n", proj.WorkerName())
	return nil
}

// parseUpArgs reads `piso up [dir] [--proxy-port N] [--ctrl-port N] [--ingress-port N]`.
// Port flags of 0 mean "keep saved / default".
func parseUpArgs(args []string) (string, pisoconfig.HostPorts, error) {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var ports pisoconfig.HostPorts
	fs.IntVar(&ports.Proxy, "proxy-port", 0, "host port for the egress proxy")
	fs.IntVar(&ports.Control, "ctrl-port", 0, "host port for the dashboard / control API")
	fs.IntVar(&ports.Ingress, "ingress-port", 0, "host port for name.piso.local ingress")
	if err := fs.Parse(args); err != nil {
		return "", pisoconfig.HostPorts{}, err
	}
	dir := "."
	if rest := fs.Args(); len(rest) > 0 {
		dir = rest[0]
	}
	return dir, ports, nil
}

// registerWorkerWithGateway tells the gateway this worker's name/slug and its
// vpc IPs, so the proxy can tag request logs with the slug. Best-effort: a
// failure here must never fail `piso up`.
func registerWorkerWithGateway(proj pisoconfig.Project) error {
	ips, err := dockernet.ContainerIPs(proj.WorkerName())
	if err != nil {
		return err
	}
	body, _ := json.Marshal(pisoconfig.WorkerIn{
		Name: proj.WorkerName(), Slug: proj.Slug, IPs: ips,
	})
	gw := pisoconfig.GatewayURL()
	resp, err := http.Post(gw + "/api/v1/workers", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gateway %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// workerImageName is the shared worker image. Compose project names differ
// per directory; the image must not, or every `piso up` in a new folder
// rebuilds (or ships an empty stub and hangs on runtime npm install).
const workerImageName = "piso-worker"

func workerNeedsBuild() bool {
	buildDir, err := pisoconfig.WorkerBuildDir()
	if err != nil {
		return true
	}
	want, err := workerhash.ContextHash(buildDir)
	if err != nil {
		return true
	}
	if hash, ok := imageLabel(workerImageName, "piso.worker.hash"); ok && hash == want {
		return false
	}
	// Reuse a per-project bake from before the image name was shared.
	if adoptWorkerImage(want) {
		return false
	}
	return true
}

func adoptWorkerImage(want string) bool {
	out, err := exec.Command("docker", "images", "-q", "--filter", "label=piso.worker.hash="+want).Output()
	if err != nil {
		return false
	}
	id := ""
	for _, line := range strings.Split(string(out), "\n") {
		if id = strings.TrimSpace(line); id != "" {
			break
		}
	}
	if id == "" {
		return false
	}
	if err := exec.Command("docker", "tag", id, workerImageName).Run(); err != nil {
		return false
	}
	return true
}

func imageLabel(name, label string) (string, bool) {
	out, err := exec.Command("docker", "inspect", "-f", "{{index .Config.Labels \""+label+"\"}}", name).Output()
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(out))
	if v == "" || v == "<no value>" {
		return "", false
	}
	return v, true
}

func syncPiProfile() error {
	dataDir, err := pisoconfig.DataDir()
	if err != nil {
		return err
	}
	agent, err := piprofile.AgentDir()
	if err != nil {
		return err
	}
	dest := piprofile.ProfileDir(dataDir)
	settings, _ := os.ReadFile(piprofile.SettingsPath(agent))
	rawModels, modelsErr := os.ReadFile(piprofile.ModelsPath(agent))
	placeholders := map[string]string{}
	if modelsErr == nil {
		keys, err := piprofile.ExtractProviderKeys(rawModels)
		if err != nil {
			return err
		}
		imported, err := importPiKeys(keys)
		if err != nil {
			return err
		}
		for _, r := range imported {
			placeholders[r.Name] = r.Placeholder
			fmt.Printf("piso: imported %s → %s on %s\n", r.Name, r.Placeholder, r.Host)
		}
	}
	var models []byte
	if modelsErr == nil {
		models, err = piprofile.SanitizeModelsJSON(rawModels, placeholders)
		if err != nil {
			return err
		}
		extracted, _ := piprofile.ExtractProviderKeys(rawModels)
		for _, k := range extracted {
			if k.APIKey != "" && strings.Contains(string(models), k.APIKey) {
				return fmt.Errorf("refusing to write profile: real key would leak")
			}
		}
	}
	return piprofile.WriteProfile(dest, settings, models)
}

type piKeyImportResult struct {
	Name        string `json:"name"`
	Placeholder string `json:"placeholder"`
	EnvKey      string `json:"envKey"`
	Host        string `json:"host"`
}

func importPiKeys(keys []piprofile.ProviderKey) ([]piKeyImportResult, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	body := map[string]any{"providers": keys}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	gw := pisoconfig.ControlAPIURL()
	resp, err := http.Post(gw+"/api/v1/imports/pi-keys", "application/json", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("import keys: %s", strings.TrimSpace(string(b)))
	}
	var out struct {
		Providers []piKeyImportResult `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Providers, nil
}

func prepareWorkerBuild() (int, error) {
	agent, err := piprofile.AgentDir()
	if err != nil {
		return 0, err
	}
	settings, _ := os.ReadFile(piprofile.SettingsPath(agent))
	pkgs, err := piprofile.ReadSettingsPackages(settings)
	if err != nil {
		return 0, err
	}
	hostRaw, _ := os.ReadFile(piprofile.HostPackageJSON(agent))
	hostDeps, err := pkgstamp.DepsFromHostPackageJSON(hostRaw)
	if err != nil {
		return 0, err
	}
	deps := pkgstamp.MergeDeps(hostDeps, pkgstamp.DepsFromPackages(pkgs))
	pkgJSON, err := pkgstamp.PackageJSON(deps)
	if err != nil {
		return 0, err
	}
	dest, err := pisoconfig.WorkerBuildDir()
	if err != nil {
		return 0, err
	}
	if err := pisoconfig.CopyWorkerSkeleton(dest); err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(dest, "package.json"), pkgJSON, 0o600); err != nil {
		return 0, fmt.Errorf("stage package.json: %w", err)
	}
	return len(deps), nil
}

func cmdDown(args []string) error {
	proj, err := pisoconfig.ProjectFromCWD()
	if err != nil {
		return err
	}
	workerFile, err := pisoconfig.WriteWorkerCompose(proj)
	if err != nil {
		return err
	}
	composeEnv, err := dockerComposeEnv()
	if err != nil {
		return err
	}
	if err := runEnv("docker", []string{"compose", "-f", workerFile, "-p", "piso-" + proj.Slug, "down"}, composeEnv); err != nil {
		return err
	}
	fmt.Println("piso: worker stopped (gateway left running)")
	return nil
}

func cmdAttach(args []string) error {
	proj, err := pisoconfig.ProjectFromCWD()
	if err != nil {
		return err
	}
	inner := "set -a; if [ -f /etc/piso/placeholders.env ]; then . /etc/piso/placeholders.env; fi; set +a; "
	if len(args) > 0 && args[0] == "--shell" {
		inner += "exec /bin/bash"
	} else {
		// pi -r is the session picker; with zero jsonl files it shows
		// "No sessions found" and waits. Start a new session instead.
		inner += `if find /root/.pi/agent/sessions -name '*.jsonl' -print -quit 2>/dev/null | grep -q .; then exec pi -r; else exec pi; fi`
	}
	cmdArgs := []string{"exec", "-it", proj.WorkerName(), "bash", "-lc", inner}
	c := exec.Command("docker", cmdArgs...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// ---- status / secrets / expose / logs / dashboard ----

func cmdStatus(args []string) error {
	gw := pisoconfig.GatewayURL()
	if healthy(gw) {
		fmt.Printf("gateway: live (%s)  dashboard: %s\n", gw, pisoconfig.DashboardURL())
	} else {
		fmt.Println("gateway: DOWN (run `piso up`)")
	}
	exists, internal, err := dockernet.InspectVPC()
	switch {
	case err != nil:
		fmt.Printf("piso_vpc: inspect failed: %v\n", err)
	case !exists:
		fmt.Println("piso_vpc: missing")
	case internal:
		fmt.Println("piso_vpc: internal")
	default:
		fmt.Println("piso_vpc: NOT internal (workers can bypass the gateway)")
	}
	out, _ := exec.Command("docker", "ps", "--format", "{{.Names}}\t{{.Status}}").Output()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.Contains(line, "piso-") {
			fmt.Println("  ", line)
		}
	}
	return nil
}

func cmdSecrets(args []string) error {
	gw := pisoconfig.GatewayURL()
	if len(args) == 0 || args[0] == "list" {
		var ss []pisoconfig.SecretSummary
		if err := getJSON(gw+"/api/v1/secrets", &ss); err != nil {
			return err
		}
		if len(ss) == 0 {
			fmt.Println("no secrets. `piso secrets add` or use the dashboard.")
			return nil
		}
		fmt.Printf("%-24s %-32s %s\n", "NAME", "PLACEHOLDER", "HOSTS")
		for _, s := range ss {
			fmt.Printf("%-24s %-32s %s\n", s.Name, s.Placeholder, strings.Join(s.AllowedHosts, ","))
		}
		return nil
	}
	switch args[0] {
	case "add":
		if len(args) < 5 {
			return fmt.Errorf("usage: piso secrets add <name> <placeholder> <value> [host,...]")
		}
		body, _ := json.Marshal(pisoconfig.SecretIn{
			Name: args[1], Placeholder: args[2], Value: args[3],
			AllowedHosts: splitCSV(args[4:]),
		})
		resp, err := http.Post(gw+"/api/v1/secrets", "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 201 {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("gateway: %s", string(b))
		}
		fmt.Println("secret added:", args[2])
		return nil
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: piso secrets rm <id>")
		}
		req, _ := http.NewRequest("DELETE", gw+"/api/v1/secrets/"+args[1], nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 204 {
			return fmt.Errorf("gateway: %d", resp.StatusCode)
		}
		fmt.Println("secret deleted")
		return nil
	default:
		return fmt.Errorf("unknown secrets subcommand %q", args[0])
	}
}

func cmdExpose(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: piso expose <port> [--name n] [--worker name]")
	}
	var port int
	if _, err := fmt.Sscanf(args[0], "%d", &port); err != nil {
		return err
	}
	name := fmt.Sprintf("p%d", port)
	worker := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) {
				name, i = args[i+1], i+1
			}
		case "--worker":
			if i+1 < len(args) {
				worker, i = args[i+1], i+1
			}
		}
	}
	if worker == "" {
		proj, err := pisoconfig.ProjectFromCWD()
		if err != nil {
			return err
		}
		worker = proj.WorkerName()
	}
	body, _ := json.Marshal(pisoconfig.RouteIn{Name: name, Worker: worker, Port: port})
	gw := pisoconfig.GatewayURL()
	resp, err := http.Post(gw+"/api/v1/routes", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gateway: %s", string(b))
	}
	if err := pisoconfig.EnsureIngressHost(name); err != nil {
		fmt.Fprintf(os.Stderr, "piso: warning: %v\n", err)
	}
	p := pisoconfig.LoadHostPorts().Ingress
	if p == 80 {
		fmt.Printf("piso: http://%s.piso.local → %s:%d\n", name, worker, port)
	} else {
		fmt.Printf("piso: http://%s.piso.local:%d → %s:%d\n", name, p, worker, port)
	}
	return nil
}

func syncIngressHosts() {
	var routes []pisoconfig.RouteIn
	if err := getJSON(pisoconfig.GatewayURL()+"/api/v1/routes", &routes); err != nil {
		return
	}
	names := make([]string, 0, len(routes))
	for _, r := range routes {
		if r.Name != "" {
			names = append(names, r.Name)
		}
	}
	if len(names) == 0 {
		return
	}
	if err := pisoconfig.EnsureIngressHosts(names); err != nil {
		fmt.Fprintf(os.Stderr, "piso: warning: %v\n", err)
	}
}

func cmdLogs(args []string) error {
	gw := pisoconfig.GatewayURL()
	follow := len(args) > 0 && args[0] == "--follow"
	if !follow {
		var recs []pisoconfig.LogRecord
		if err := getJSON(gw+"/api/v1/requests", &recs); err != nil {
			return err
		}
		printRecords(recs)
		return nil
	}
	// SSE stream
	req, _ := http.NewRequest("GET", gw+"/api/v1/requests/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var rec pisoconfig.LogRecord
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &rec); err == nil {
				printRecord(rec)
			}
		}
	}
	return sc.Err()
}

func cmdDashboard(args []string) error {
	gw := pisoconfig.DashboardURL()
	fmt.Println("opening", gw)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", gw)
	case "linux":
		cmd = exec.Command("xdg-open", gw)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	return cmd.Start()
}

// cmdSetup is the post-install hook. `make install` calls it as the login
// user (never as root) so ~/.piso stays owned by that user and Docker
// Desktop / OrbStack sockets remain reachable.
func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rebuild := fs.Bool("rebuild", false, "rebuild and recreate the gateway container")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dst, err := pisoconfig.DataDir()
	if err != nil {
		return err
	}
	if err := pisoconfig.EnsurePlaceholdersEnv(); err != nil {
		return err
	}
	if from := strings.TrimSpace(os.Getenv("PISO_MIGRATE_FROM")); from != "" {
		copied, migErr := pisoconfig.ImportLegacyData(from, dst)
		if migErr != nil {
			return fmt.Errorf("migrate data: %w", migErr)
		}
		if len(copied) > 0 {
			fmt.Printf("piso: imported %s from %s\n", strings.Join(copied, ", "), from)
		} else {
			fmt.Printf("piso: data dir %s already present (no files imported)\n", dst)
		}
	}

	// Persist defaults if ports.json is missing so compose interpolation
	// matches the next gateway recreate.
	if _, err := pisoconfig.ResolveHostPorts(pisoconfig.HostPorts{}); err != nil {
		return err
	}

	if err := pisoconfig.EnsurePisoLocal(); err != nil {
		fmt.Fprintf(os.Stderr, "piso: warning: %v\n", err)
	}

	if !*rebuild {
		return nil
	}
	return rebuildGateway()
}

// rebuildGateway force-rebuilds the gateway image and recreates its
// container. The gateway store migrates state.json on startup. Workers
// are left running.
func rebuildGateway() error {
	ports := pisoconfig.LoadHostPorts()
	if !healthy(pisoconfig.ControlAPIURL()) {
		if err := pisoconfig.CheckHostPortsFree(ports); err != nil {
			return err
		}
	}
	if err := startGateway(true); err != nil {
		return err
	}
	if err := waitGatewayHealthy(); err != nil {
		return err
	}
	fmt.Println("piso: gateway rebuilt")
	return nil
}

// ---- shared helpers ----

// startGateway brings up the shared gateway compose project. forceRecreate
// rebuilds the image and replaces the container so `make install` picks up
// new gateway code and runs store migrations against ~/.piso/state.json.
func startGateway(forceRecreate bool) error {
	if err := dockernet.EnsureInternal(); err != nil {
		return err
	}
	gatewayFile, err := pisoconfig.GatewayCompose()
	if err != nil {
		return err
	}
	composeEnv, err := dockerComposeEnv()
	if err != nil {
		return err
	}
	args := []string{"compose", "-f", gatewayFile, "-p", "piso", "up", "-d", "--build", "-t", "0"}
	if forceRecreate {
		args = append(args, "--force-recreate")
	}
	if err := runEnv("docker", args, composeEnv); err != nil {
		return fmt.Errorf("gateway up: %w", err)
	}
	return dockernet.AssertInternal()
}

// waitGatewayHealthy polls the control plane until it answers or 30s elapses.
func waitGatewayHealthy() error {
	gw := pisoconfig.ControlAPIURL()
	dash := pisoconfig.DashboardURL()
	for i := 0; i < 30; i++ {
		if healthy(gw) {
			fmt.Printf("piso: gateway live — dashboard: %s\n", dash)
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("gateway did not become healthy within 30s")
}

// dockerComposeEnv is required for compose interpolation of ${PISO_DATA}
// (gateway bind-mount) and for BuildKit on image builds.
func dockerComposeEnv() (map[string]string, error) {
	data, err := pisoconfig.DataDir()
	if err != nil {
		return nil, err
	}
	p := pisoconfig.LoadHostPorts()
	return map[string]string{
		"DOCKER_BUILDKIT":   "1",
		"PISO_DATA":         data,
		"PISO_PROXY_PORT":   fmt.Sprintf("%d", p.Proxy),
		"PISO_CTRL_PORT":    fmt.Sprintf("%d", p.Control),
		"PISO_INGRESS_PORT": fmt.Sprintf("%d", p.Ingress),
	}, nil
}

func healthy(gw string) bool {
	resp, err := http.Get(gw + "/api/v1/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func getJSON(url string, v any) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("gateway %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func printRecords(recs []pisoconfig.LogRecord) {
	for _, r := range recs {
		printRecord(r)
	}
}

func printRecord(r pisoconfig.LogRecord) {
	f := ""
	if len(r.Findings) > 0 {
		var parts []string
		for _, x := range r.Findings {
			parts = append(parts, x.Kind)
		}
		f = " [" + strings.Join(parts, ",") + "]"
	}
	slug := strings.TrimSpace(r.Slug)
	if slug == "" {
		slug = strings.TrimSpace(r.Worker)
	}
	if slug == "" {
		slug = "?"
	}
	fmt.Printf("%s %-8s %-11s %-3d %-5s %s://%s%s  %s%s\n",
		r.Ts.Format("15:04:05"), slug, r.Action, r.Status, r.Method,
		r.Scheme, r.Host, r.Path, strings.Join(r.Reasons, "+"), f)
}

func splitCSV(parts []string) []string {
	var out []string
	for _, p := range parts {
		for _, s := range strings.Split(p, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func run(name string, args ...string) error {
	return runEnv(name, args, nil)
}

func runEnv(name string, args []string, env map[string]string) error {
	if name == "docker" {
		resolved, err := dockernet.LookPath()
		if err != nil {
			return err
		}
		name = resolved
	}
	// Worker image bake (apt + bun + pi + host npm) should finish well
	// under this; CommandContext SIGKILLs docker if it does not.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if env != nil {
		c.Env = append(os.Environ(), envPairs(env)...)
	}
	if err := c.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out after 20m running %s %s", name, strings.Join(args, " "))
		}
		return err
	}
	return nil
}

func envPairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
