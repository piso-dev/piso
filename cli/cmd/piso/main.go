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
	case "sync":
		err = cmdSync(args)
	case "logs":
		err = cmdLogs(args)
	case "dashboard":
		err = cmdDashboard(args)
	case "setup":
		err = cmdSetup(args)
	case "update":
		err = cmdUpdate(args)
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
  piso attach [--shell] [--new] [--session <id|path>]
                         enter the worker and run pi (new session if none exist;
                         --new forces a fresh one, --session opens a specific
                         one, --shell drops into bash)
  piso status            show gateway + worker state
  piso secrets list|add|rm   manage gateway secrets (real values never leave it)
  piso expose <port> [--name n]  reverse-proxy a worker port as https://n.piso.local
  piso sync [--watch]     reconcile /etc/hosts with gateway routes; --watch keeps
                           syncing live as auto routes appear/expire
  piso sync daemon[-status|-restart|-stop|-uninstall]
                           manage the global hosts-sync service (auto-starts on
                           piso up; run restart manually after a sudo make install)
  piso logs [--follow]   tail the gateway request log (SSE when --follow)
  piso dashboard         open the gateway web UI (http://piso.local)
  piso setup [--rebuild] import leftover data and (with --rebuild) recreate the gateway
  piso update [version] [--dry-run] [--force]  refresh host extensions, pin a pi version,
                         rebuild the shared worker image once, and recreate every worker
                         including the monitor (its in-flight pi is killed; user attaches
                         still block). Confirm before applying.

Host ports default to 8080 (proxy) and 80 (single web port: dashboard at
http://piso.local AND every route at http://<label>.piso.local, dispatched by
Host — no port in any URL). 8082 remains as the legacy ingress alias.
On Windows the control port defaults to 8081 (port 80 is often reserved).
If a port is taken, piso up exits with the flag to override
(--proxy-port, --ctrl-port, --ingress-port). Env: PISO_*_PORT.
make install (Unix) or scripts/install.ps1 (Windows) runs piso setup --rebuild
so an existing install is replaced in place (binaries, share tree, gateway
image, and state.json schema).
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

	if err := dockernet.AssertLinuxEngine(); err != nil {
		return err
	}
	ports, err := pisoconfig.ResolveHostPorts(cliPorts)
	if err != nil {
		return err
	}
	if err := pisoconfig.EnsurePisoLocal(); err != nil {
		fmt.Fprintf(os.Stderr, "piso: warning: %v\n", err)
		fmt.Fprintf(os.Stderr, "piso: dashboard loopback: %s\n", pisoconfig.ControlAPIURL())
	}
	if err := pisoconfig.EnsureWorkerPlaceholdersEnv(proj.Slug); err != nil {
		return err
	}
	// The monitor also gets a placeholders.env (its scoped model key is in the
	// secrets vault; this exports piso_monitor_… so the monitor's pi can call
	// the model through the MITM).
	if err := pisoconfig.EnsureWorkerPlaceholdersEnv("monitor"); err != nil {
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

	// 0. Build the shared worker image BEFORE the gateway: the gateway compose
	// includes a monitor service that runs `image: piso-worker`, so the image
	// must exist before `docker compose up -f gateway.yaml` creates it. On a
	// fresh machine this is the first build; otherwise it's a cheap no-op.
	if n, err := prepareWorkerBuild(); err != nil {
		return err
	} else if n > 0 {
		fmt.Printf("piso: baking %d host extensions into the worker image\n", n)
	}
	if workerNeedsBuild() {
		fmt.Println("piso: building shared worker image")
		if err := buildSharedWorker(); err != nil {
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
	if err := syncIngressHosts(); err != nil {
		fmt.Fprintf(os.Stderr, "piso: warning: hosts sync: %v\n", err)
	}
	if err := syncPiProfile(); err != nil {
		return err
	}
	// 2. worker (per-project). The image is already built (step 0); just render
	// the project's compose and create the container. After buildSharedWorker
	// stamps piso.worker.hash, workerNeedsBuild() is false and we must not
	// pass --build: compose uses a different cache key than a bare docker
	// build unless networks match, and would redo apt/bun/npm.
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
	// Compose up returns when the container is created, NOT when the
	// entrypoint has copied ~650MB of extensions into a fresh agent volume.
	// Attach before that finishes is the first-start exit 137.
	if err := waitWorkerReady(proj.WorkerName()); err != nil {
		return err
	}
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
		Name: proj.WorkerName(), Slug: proj.Slug, Dir: proj.Dir, IPs: ips,
	})
	gw := pisoconfig.GatewayURL()
	resp, err := http.Post(gw+"/api/v1/workers", "application/json", bytes.NewReader(body))
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
	// Inject the informant prompt template so the worker's pi loads the Tier B
	// convention automatically (emit progress/milestone/etc.). Preserves the
	// user's other settings; dedupes.
	settings = piprofile.EnsureInformantPrompts(settings)
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
	// --shell drops into bash instead of pi; --new always starts a fresh pi
	// session instead of the restore picker; --session <id|path> opens a
	// specific session (id or .jsonl path, resolved by pi). --shell wins if
	// combined with anything else; --new conflicts with --session.
	var shell bool
	var fresh bool
	var sess string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--shell":
			shell = true
		case "--new":
			fresh = true
		case "--session":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return fmt.Errorf("usage: piso attach [--shell] [--new] [--session <id|path>]")
			}
			sess, i = args[i+1], i+1
		}
	}
	if sess != "" && fresh {
		return fmt.Errorf("--new and --session are mutually exclusive")
	}
	inner := "set -a; if [ -f /etc/piso/placeholders.env ]; then . /etc/piso/placeholders.env; fi; set +a; "
	if shell {
		inner += "exec /bin/bash"
	} else if sess != "" {
		inner += "exec pi --session " + shq(sess)
	} else if fresh {
		inner += "exec pi"
	} else {
		// pi -r is the session picker for the CURRENT project. Sessions are
		// namespaced by cwd under ~/.pi/agent/sessions/<--cwd-->/ (pi's
		// sanitization: --<cwd with / and : → - >--). The worker always mounts
		// the project at /workspace → dir "--workspace--". Only show the
		// picker when THIS project has sessions; a jsonl under a *different*
		// project's dir must not trigger pi -r (which would open an empty
		// picker for the new project).
		inner += `sd=/root/.pi/agent/sessions/--workspace--; if [ -d "$sd" ] && find "$sd" -name '*.jsonl' -print -quit 2>/dev/null | grep -q .; then exec pi -r; else exec pi; fi`
	}
	if err := waitWorkerReady(proj.WorkerName()); err != nil {
		return err
	}
	cmdArgs := []string{"exec", "-it", proj.WorkerName(), "bash", "-lc", inner}
	bin, err := dockernet.LookPath()
	if err != nil {
		return err
	}
	c := exec.Command(bin, cmdArgs...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return attachExecErr(c.Run())
}

// attachExecErr rewrites docker-exec deaths that are otherwise just
// "exit status 137". 137 is 128+SIGKILL: the OOM killer, not a pi error.
func attachExecErr(err error) error {
	if err == nil {
		return nil
	}
	if isKilled137(err) {
		return fmt.Errorf("pi was killed (exit 137 / SIGKILL). First attach after a fresh worker often OOMs while ~650MB of extensions are still seeding; retry attach, or raise Docker's memory limit")
	}
	return err
}

func isKilled137(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	// docker exec maps SIGKILL to 137 (128+9). A direct child reports the
	// signal instead (ExitCode -1). processSignaledKill is Unix-only.
	if ee.ExitCode() == 137 {
		return true
	}
	return processSignaledKill(ee)
}

// shq single-quotes s so it embeds losslessly in the worker's bash -lc
// string (a session id never needs it, but paths with spaces/quotes do).
func shq(s string) string {
	return "'" + strings.Join(strings.Split(s, "'"), "'\\''") + "'"
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
	case "add-monitor":
		// One-command provisioning of the monitor's scoped model key:
		//   piso secrets add-monitor api.anthropic.com sk-ant-...
		if len(args) < 3 {
			return fmt.Errorf("usage: piso secrets add-monitor <host> <value>")
		}
		host := args[1]
		value := args[2]
		ph := "piso_monitor_" + strings.ToLower(fmt.Sprintf("%x", time.Now().UnixNano()))
		body, _ := json.Marshal(pisoconfig.SecretIn{
			Name: "monitor-model", Placeholder: ph, Value: value,
			AllowedHosts: []string{host},
			Workers:      []string{"monitor"},
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
		fmt.Println("monitor secret added:", ph, "(scoped to workers: monitor; rule:", host+")")
		return nil
	case "add":
		if len(args) < 4 {
			return fmt.Errorf("usage: piso secrets add <name> <placeholder> <value> [host,...] [--workers a,b]")
		}
		name := args[1]
		ph := args[2]
		val := args[3]
		var hosts []string
		var workers []string
		i := 4
		for i < len(args) {
			switch args[i] {
			case "--workers":
				if i+1 < len(args) {
					workers = splitCSV(args[i+1 : i+2])
					i++
				}
			default:
				hosts = append(hosts, args[i])
			}
			i++
		}
		body, _ := json.Marshal(pisoconfig.SecretIn{
			Name: name, Placeholder: ph, Value: val,
			AllowedHosts: hosts,
			Workers:      workers,
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
		scope := ""
		if len(workers) > 0 {
			scope = " (workers: " + strings.Join(workers, ",") + ")"
		}
		rule := "*"
		if len(hosts) > 0 {
			rule = strings.Join(hosts, ",")
		}
		fmt.Println("secret added:", ph, scope, "(rule:", rule+")")
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
	if err := syncIngressHosts(); err != nil {
		fmt.Fprintf(os.Stderr, "piso: warning: %v\n", err)
	}
	// The web entrypoint (WebHandler) serves both dashboard and routes on the
	// CONTROL host port, dispatched by Host — so a route's canonical URL is
	// portless (http://<name>.piso.local) or uses that port when =/= 80.
	p := pisoconfig.LoadHostPorts().Control
	if p == 80 {
		fmt.Printf("piso: http://%s.piso.local → %s:%d\n", name, worker, port)
	} else {
		fmt.Printf("piso: http://%s.piso.local:%d → %s:%d\n", name, p, worker, port)
	}
	return nil
}

// syncIngressHosts reconciles /etc/hosts with the gateway's current route
// labels (adds missing, drops routed-away ones). Best-effort at `piso up`/
// `piso expose` (callers print the error as a warning); fatal in `piso sync`.
func syncIngressHosts() error {
	var routes []pisoconfig.RouteRec
	if err := getJSON(pisoconfig.GatewayURL()+"/api/v1/routes", &routes); err != nil {
		return err
	}
	names := []string{"mcp-oauth"}
	for _, r := range routes {
		if r.Name != "" {
			names = append(names, r.Name)
		}
	}
	return pisoconfig.ReconcileIngressHosts(names)
}

// cmdSync reconciles /etc/hosts with the gateway's routes.
//
//	piso sync                     one-shot reconcile
//	piso sync --watch             foreground watch loop (SSE route events)
//	piso sync daemon              same loop, as the managed service body
//	piso sync daemon-status       pidfile+ps probe (no root needed)
//	piso sync daemon-restart      install/reload the global service (root)
//	piso sync daemon-stop         stop it (root)
//	piso sync daemon-uninstall    stop + remove managed config (root)
func cmdSync(args []string) error {
	if len(args) == 0 {
		if err := syncIngressHosts(); err != nil {
			return err
		}
		fmt.Println("piso: hosts in sync")
		return nil
	}
	switch args[0] {
	case "--watch":
		if err := syncIngressHosts(); err != nil {
			return err
		}
		fmt.Println("piso: hosts in sync")
		return syncWatchLoop(pisoconfig.GatewayURL())
	case "daemon":
		// Service body (root): block until the gateway is reachable, then
		// reconcile and watch forever. A launchd KeepAlive restart after a
		// reboot lands here with the gateway still starting.
		gw := pisoconfig.GatewayURL()
		for {
			if err := syncIngressHosts(); err == nil {
				break
			}
			time.Sleep(5 * time.Second)
		}
		return syncWatchLoop(gw)
	case "daemon-status":
		st, err := pisoconfig.SyncDaemonStatus()
		if err != nil {
			return err
		}
		if st.Running {
			fmt.Printf("piso: hosts sync daemon: running (pid %s)\n", st.Pid)
		} else {
			fmt.Println("piso: hosts sync daemon: stopped")
		}
		return nil
	case "daemon-restart":
		fmt.Println("piso: hosts sync daemon: (re)starting…")
		return pisoconfig.SyncDaemonInstallRestart()
	case "daemon-stop":
		return pisoconfig.SyncDaemonStop()
	case "daemon-uninstall":
		return pisoconfig.SyncDaemonUninstall()
	default:
		return fmt.Errorf("usage: piso sync [--watch|daemon|daemon-status|daemon-restart|daemon-stop|daemon-uninstall]")
	}
}

// ensureSyncDaemon converges the host on the global hosts-sync service.
// Called by `piso up` (after the gateway is healthy) for EVERY project — the
// daemon is host-scoped, not per-worker, so different projects share the one
// service. Running → leave it; stopped → install/restart it. Errors are the
// caller's to downgrade (piso up warns instead of failing).
func ensureSyncDaemon() error {
	if !pisoconfig.SyncDaemonSupported() {
		return pisoconfig.ErrSyncDaemonUnsupported
	}
	st, err := pisoconfig.SyncDaemonStatus()
	if err != nil {
		return err
	}
	if st.Running {
		fmt.Printf("piso: hosts sync daemon: running (pid %s)\n", st.Pid)
		return nil
	}
	fmt.Println("piso: hosts sync daemon: not running — (re)starting (may prompt for sudo)")
	return pisoconfig.SyncDaemonInstallRestart()
}

// syncWatchLoop reconciles /etc/hosts whenever a route event arrives on the
// gateway SSE stream (new auto route, route deleted, stale route swept). Each
// (re)connect reconciles once, so events missed while disconnected are
// recovered; the loop only returns when the process is killed. Idempotent
// ReconcileIngressHosts makes the event-driven calls cheap.
func syncWatchLoop(gw string) error {
	for {
		req, _ := http.NewRequest("GET", gw+"/api/v1/requests/stream", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if err := syncIngressHosts(); err != nil {
			fmt.Fprintf(os.Stderr, "piso: warning: %v\n", err)
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			// route + planning events carry "port"; request-log records do not.
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"port"`) {
				if err := syncIngressHosts(); err != nil {
					fmt.Fprintf(os.Stderr, "piso: warning: %v\n", err)
				}
			}
		}
		resp.Body.Close()
		time.Sleep(2 * time.Second)
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
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", gw)
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
	if err := dockernet.AssertLinuxEngine(); err != nil {
		return err
	}
	ports := pisoconfig.LoadHostPorts()
	if !healthy(pisoconfig.ControlAPIURL()) {
		if err := pisoconfig.CheckHostPortsFree(ports); err != nil {
			return err
		}
	}
	// The gateway compose now includes the monitor (image: piso-worker); the
	// shared worker image must exist before `docker compose up` creates it.
	if _, err := prepareWorkerBuild(); err != nil {
		return err
	}
	if workerNeedsBuild() {
		if err := buildSharedWorker(); err != nil {
			return err
		}
	}
	// The monitor needs its placeholders.env (scoped model key) too.
	if err := pisoconfig.EnsureWorkerPlaceholdersEnv("monitor"); err != nil {
		return err
	}
	if err := startGateway(true); err != nil {
		return err
	}
	if err := waitGatewayHealthy(); err != nil {
		return err
	}
	fmt.Println("piso: gateway rebuilt")
	printTrustCAHint()
	return nil
}

func printTrustCAHint() {
	if runtime.GOOS != "windows" {
		return
	}
	dir, err := pisoconfig.DataDir()
	if err != nil {
		return
	}
	fmt.Printf("piso: trust the MITM CA in Windows (needed for https://*.piso.local):\n  certutil -addstore -user Root %s\n", filepath.Join(dir, "ca.crt"))
}

// updateDecision is the cmdUpdate gate: for a given pinned/current pi version,
// force flag, and package-set before/after the extension refresh, decide whether
// to proceed. Exported as a pure function so the logic is unit-testable without
// npm/docker round-trips.
type updateDecision int

const (
	updateDecisionNothing  = 0 // already current and nothing changed
	updateDecisionPackages = 1 // extensions changed — re-stage + rebuild
	updateDecisionRollPi   = 2 // pi pin changed — normal rollout
)

// decideUpdate decides the cmdUpdate gate: nothing to do vs proceed.
func decideUpdate(old, version string, force, pkgsOk bool, pkgsBefore, pkgsAfter string) updateDecision {
	if old != version {
		return updateDecisionRollPi
	}
	if force {
		return updateDecisionRollPi
	}
	if pkgsOk && pkgsBefore != "" && pkgsAfter != "" && pkgsBefore != pkgsAfter {
		return updateDecisionPackages
	}
	return updateDecisionNothing
}

// cmdUpdate rolls out a pinned pi version to all workers: resolve the target
// version, pin it in the staged worker Dockerfile (which changes the build
// hash), rebuild the shared piso-worker image once, and recreate each worker.
func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "show what would change without building")
	force := fs.Bool("force", false, "rebuild and recreate even when the staged version already matches")
	if err := fs.Parse(args); err != nil {
		return err
	}
	version := strings.TrimSpace(fs.Arg(0))

	// 0. hash the CURRENT package set BEFORE refreshing extensions so we can
	// tell whether the refresh changed anything (a package-only update must
	// still roll to workers even when the pi pin is already current).
	pkgsBefore, pkgErr := stagedPackagesHash()
	if pkgErr != nil {
		// Not fatal: if we cannot hash the pre-refresh state we conservatively
		// treat it as "no package change" so the pi-pin (or --force) decides;
		// the updateDecision guard below stays Nothing for packages.
		pkgsBefore = ""
	}
	// 0. refresh extensions on the host first: updates ~/.pi/agent/npm/
	// package.json (+ settings packages), which piso re-stages into the worker
	// build context — so the rebuilt image bakes the newer extension versions
	// alongside the new pi. Best-effort: a failure here is a warning, not fatal
	// (the pi pin itself can still roll out).
	if !*dryRun {
		if err := updateHostExtensions(); err != nil {
			fmt.Fprintf(os.Stderr, "piso: warning: extension refresh failed: %v\n", err)
		} else {
			fmt.Println("piso: refreshed host extensions")
		}
	}

	// 1. resolve the target version (latest from npm when not specified)
	if version == "" {
		v, err := npmLatestPiVersion()
		if err != nil {
			return fmt.Errorf("resolve latest pi: %w", err)
		}
		version = v
		fmt.Printf("piso: latest pi is %s\n", version)
	} else {
		fmt.Printf("piso: pinning pi %s\n", version)
	}

	// 2. rewrite the staged Dockerfile's ARG (staged copy, not the repo)
	buildDir, err := pisoconfig.WorkerBuildDir()
	if err != nil {
		return err
	}
	dfPath := filepath.Join(buildDir, "Dockerfile")
	df, err := os.ReadFile(dfPath)
	if err != nil {
		return fmt.Errorf("staged Dockerfile: %w (run piso up first)", err)
	}
	if !bytes.Contains(df, []byte("PISO_PI_VERSION")) {
		return fmt.Errorf("staged Dockerfile missing PISO_PI_VERSION arg; re-run piso up to re-stage")
	}

	// current pinned version (persisted; the staged Dockerfile is ephemeral)
	old := pisoconfig.LoadPiVersion()
	pkgsAfter, _ := stagedPackagesHash()
	enough := decideUpdate(old, version, *force, pkgErr == nil, pkgsBefore, pkgsAfter)
	if enough == updateDecisionNothing {
		fmt.Printf("piso: already on pi %s (use --force to rebuild anyway)\n", version)
		return nil
	}
	if enough == updateDecisionPackages {
		fmt.Println("piso: extension packages changed — rebuilding to bake them")
	}
	if old == version && *force {
		fmt.Printf("piso: already on pi %s — forcing build + recreate\n", version)
	}

	// 2b. fail-closed session guard: probe every running worker for a live pi.
	// An in-flight user attach dies when the container is recreated, so abort
	// before any build or confirm. The monitor's pi -p is not a user session:
	// it shares the worker image and must be recreated, so it is killed as
	// part of the rollout. --force does not override a user attach (there is
	// no --kill-sessions). In dry-run the same scan runs but only reports.
	var recs []pisoconfig.WorkerRec
	if err := getJSON(pisoconfig.GatewayURL()+"/api/v1/workers", &recs); err != nil {
		return fmt.Errorf("list workers: %w", err)
	}
	recs = ensureMonitorRec(recs)
	sessions := scanActivePiSessions(recs)
	busy := blockingSessions(sessions)
	if mons := monitorSessions(sessions); len(mons) > 0 {
		fmt.Println("piso: monitor pi will be restarted with the new image:")
		for _, s := range mons {
			fmt.Printf("  %s (%s): pi PIDs %s, up %s\n", s.Worker, s.Slug, strings.Join(s.PIDs, ","), s.Uptime)
		}
	}
	if len(busy) > 0 {
		fmt.Println("piso: active pi session(s) detected:")
		for _, s := range busy {
			fmt.Printf("  %s (%s): pi PIDs %s, up %s\n", s.Worker, s.Slug, strings.Join(s.PIDs, ","), s.Uptime)
		}
		if *dryRun {
			fmt.Printf("piso: --dry-run: would NOT proceed (%d worker(s) busy)\n", len(busy))
			return nil
		}
		return fmt.Errorf("active pi session(s) in %d worker(s); close pi and re-run (--force does not kill user sessions)", len(busy))
	}

	if *dryRun {
		what := fmt.Sprintf("pin %s → %s", old, version)
		if enough == updateDecisionPackages {
			what += " + refreshed extension packages"
		}
		fmt.Printf("piso: --dry-run: would %s, rebuild piso-worker, recreate workers including monitor\n", what)
		return nil
	}

	// 3. confirm before applying (explicit version skips the prompt)
	if fs.Arg(0) == "" {
		what := fmt.Sprintf("Roll out pi %s → %s", old, version)
		if enough == updateDecisionPackages {
			what += " + refreshed extension packages"
		}
		if !confirm(what + " to all workers") {
			fmt.Println("piso: cancelled")
			return nil
		}
	}

	// 4. persist the pin + rewrite the staged Dockerfile ARG
	if err := pisoconfig.SavePiVersion(version); err != nil {
		return fmt.Errorf("save pi version: %w", err)
	}
	updated := pisoconfig.RewriteWorkerDockerfileARG(string(df), version)
	if err := os.WriteFile(dfPath, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write staged Dockerfile: %w", err)
	}
	fmt.Printf("piso: staged pi %s in worker build context\n", version)

	// 4b. re-stage the extension package.json (fresh pins after updateHostExtensions)
	// + re-stage the Dockerfile (CopyWorkerSkeleton re-injects the persisted pi
	// pin). The rebuild hash then covers BOTH the pi pin and extension pins.
	if _, err := prepareWorkerBuild(); err != nil {
		return fmt.Errorf("re-stage worker build: %w", err)
	}

	// 5. enumerate workers + build + recreate
	return rolloutWorkers(old, version)
}

// updateHostExtensions refreshes the host's installed pi extensions by running
// `pi update --extensions`. This updates ~/.pi/agent/npm/package.json (plus the
// settings.json package pins), which the next image build re-stages into the
// worker — so the rebuilt worker ships the newer extension versions alongside
// the new pi. Best-effort: returns an error (treated as a warning by callers)
// if pi is missing or the update fails.
func updateHostExtensions() error {
	piBin, err := exec.LookPath("pi")
	if err != nil {
		return fmt.Errorf("pi not on PATH: %w", err)
	}
	return exec.Command(piBin, "update", "--extensions").Run()
}

// stagedPackagesHash returns the hash of the extension package set that WOULD
// be staged into the worker build context (host package.json + settings
// packages merged). Read-only: does not write or re-stage. Used by cmdUpdate to
// detect whether a package-only refresh changed anything, so the early-return
// "already on pi X" does not silently skip rolling new packages to workers.
func stagedPackagesHash() (string, error) {
	agent, err := piprofile.AgentDir()
	if err != nil {
		return "", err
	}
	settings, _ := os.ReadFile(piprofile.SettingsPath(agent))
	pkgs, err := piprofile.ReadSettingsPackages(settings)
	if err != nil {
		return "", err
	}
	hostRaw, _ := os.ReadFile(piprofile.HostPackageJSON(agent))
	hostDeps, err := pkgstamp.DepsFromHostPackageJSON(hostRaw)
	if err != nil {
		return "", err
	}
	deps := pkgstamp.MergeDeps(hostDeps, pkgstamp.DepsFromPackages(pkgs))
	return pkgstamp.Hash(deps), nil
}

// npmLatestPiVersion queries the npm registry for the latest pi version.
func npmLatestPiVersion() (string, error) {
	resp, err := http.Get("https://registry.npmjs.org/@earendil-works/pi-coding-agent/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var d struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return "", err
	}
	if d.Version == "" {
		return "", fmt.Errorf("npm gave empty version")
	}
	return d.Version, nil
}

// confirm asks a yes/no on stdin (no TTY → default no).
func confirm(prompt string) bool {
	fmt.Printf("%s [y/N]: ", prompt)
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		ans := strings.ToLower(strings.TrimSpace(sc.Text()))
		return ans == "y" || ans == "yes"
	}
	return false
}

// rolloutWorkers enumerates registered workers, builds the shared worker image
// once (the staged ARG change makes workerNeedsBuild fire), recreates each
// project worker via its compose file, then recreates the monitor (same image,
// gateway compose). Skips workers that aren't running. User attaches were
// already gated in cmdUpdate; the monitor's pi -p is killed by the recreate.
func rolloutWorkers(old, version string) error {
	// enumerate registered workers
	var recs []pisoconfig.WorkerRec
	if err := getJSON(pisoconfig.GatewayURL()+"/api/v1/workers", &recs); err != nil {
		return fmt.Errorf("list workers: %w", err)
	}

	// build the shared image once (hash change → --build)
	if err := buildSharedWorker(); err != nil {
		return err
	}
	fmt.Printf("piso: built piso-worker (pi %s → %s)\n", old, version)

	// recreate each registered running project worker
	updated := 0
	for _, w := range recs {
		if isMonitorWorker(w.Name, w.Slug) {
			continue // gateway compose, not a per-project file; see recreateMonitor
		}
		if w.Dir == "" {
			fmt.Printf("  %s: no project dir registered, skipping\n", w.Name)
			continue
		}
		if !containerRunning(w.Name) {
			fmt.Printf("  %s: not running, skipping\n", w.Name)
			continue
		}
		if err := recreateWorker(w); err != nil {
			fmt.Printf("  %s: recreate failed: %v\n", w.Name, err)
			continue
		}
		fmt.Printf("  %s: recreated\n", w.Name)
		updated++
	}
	if !containerRunning("piso-gateway") {
		fmt.Printf("  %s: gateway not running, skipping\n", monitorWorkerName)
	} else if err := recreateMonitor(); err != nil {
		fmt.Printf("  %s: recreate failed: %v\n", monitorWorkerName, err)
	} else {
		fmt.Printf("  %s: recreated\n", monitorWorkerName)
		updated++
	}
	fmt.Printf("piso: rollout complete (%d workers recreated)\n", updated)
	return nil
}

// workerBuildArgs is the `docker build` argv for the shared piso-worker image.
// PISO_WORKER_HASH must be passed so the image label matches ContextHash;
// otherwise workerNeedsBuild() stays true (label defaults to "dev") and
// `piso up` runs a second compose --build. --network=host matches
// compose/worker.yaml.tmpl so the two builders share BuildKit cache.
func workerBuildArgs(buildDir, hash string) []string {
	return []string{
		"build",
		"--network=host",
		"-t", workerImageName,
		"--build-arg", "PISO_WORKER_HASH=" + hash,
		buildDir,
	}
}

// buildSharedWorker rebuilds piso-worker from the staged context and stamps
// piso.worker.hash so the next workerNeedsBuild() check is a no-op.
func buildSharedWorker() error {
	composeEnv, err := dockerComposeEnv()
	if err != nil {
		return err
	}
	buildDir, err := pisoconfig.WorkerBuildDir()
	if err != nil {
		return err
	}
	hash, err := workerhash.ContextHash(buildDir)
	if err != nil {
		return fmt.Errorf("worker context hash: %w", err)
	}
	return runEnv("docker", workerBuildArgs(buildDir, hash), composeEnv)
}

// containerRunning reports whether a named container is up.
func containerRunning(name string) bool {
	out, err := dockerOutputLite("ps", "--filter", "name="+name, "--filter", "status=running", "--format", "{{.Names}}")
	if err != nil {
		return false
	}
	return strings.Contains(out, name)
}

// recreateWorker recreates a worker's compose service (image change → recreate).
func recreateWorker(w pisoconfig.WorkerRec) error {
	composeEnv, err := dockerComposeEnv()
	if err != nil {
		return err
	}
	file, err := pisoconfig.WriteWorkerCompose(pisoconfig.Project{Dir: w.Dir, Slug: w.Slug})
	if err != nil {
		return err
	}
	return runEnv("docker", []string{"compose", "-f", file, "-p", "piso-" + w.Slug, "up", "-d", "-t", "0"}, composeEnv)
}

// recreateMonitor replaces the project-manager container from the gateway
// compose project. --no-deps leaves piso-gateway alone; --force-recreate +
// -t 0 picks up the rebuilt piso-worker image and kills any in-flight pi -p.
func recreateMonitor() error {
	if err := pisoconfig.EnsureWorkerPlaceholdersEnv(monitorSlug); err != nil {
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
	return runEnv("docker", []string{
		"compose", "-f", gatewayFile, "-p", "piso",
		"up", "-d", "-t", "0", "--no-deps", "--force-recreate", "monitor",
	}, composeEnv)
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

const workerReadyTimeout = 2 * time.Minute

// waitWorkerReady blocks until the entrypoint has finished seeding (extension
// copy, node-gyp headers, identity checkin) and exec'd CMD. `docker compose up`
// returns earlier; attaching during the copy is the first-start exit 137.
func waitWorkerReady(name string) error {
	deadline := time.Now().Add(workerReadyTimeout)
	started := time.Now()
	var announced bool
	for time.Now().Before(deadline) {
		if !containerRunning(name) {
			if time.Since(started) > 5*time.Second {
				return fmt.Errorf("worker %s is not running (run `piso up` first)", name)
			}
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if workerIsReady(name) {
			return nil
		}
		if !announced {
			fmt.Fprintf(os.Stderr, "piso: waiting for %s to finish seeding extensions\n", name)
			announced = true
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("worker %s did not finish seeding within %s; check `docker logs %s`", name, workerReadyTimeout, name)
}

// workerIsReady is true once /run/piso-ready exists (new images) or once the
// entrypoint process has exec'd CMD (old images that never write the flag).
func workerIsReady(name string) bool {
	if _, err := dockerOutputLite("exec", name, "test", "-f", "/run/piso-ready"); err == nil {
		return true
	}
	_, err := dockerOutputLite("exec", name, "python3", "-c", `import os, sys
for p in os.listdir("/proc"):
    if not p.isdigit():
        continue
    try:
        cmd = open("/proc/%s/cmdline" % p, "rb").read().replace(b"\0", b" ").decode("utf-8", "replace")
    except Exception:
        continue
    if "piso-entrypoint" in cmd:
        sys.exit(1)
`)
	return err == nil
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
		"PISO_DATA":         pisoconfig.ComposeHostPath(data),
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
