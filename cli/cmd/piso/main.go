// Command piso is the piso CLI: `piso up` starts gateway + worker for the
// current directory, `piso attach` enters the worker running pi, and the rest
// control the gateway (secrets, expose, logs, dashboard).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	"piso/cli/internal/pisoconfig"
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
  piso up [dir]          ensure gateway + worker for [dir] (default: cwd)
  piso down              stop this project's worker (gateway stays up)
  piso attach            enter the worker and run pi (resume last session)
  piso status            show gateway + worker state
  piso secrets list|add|rm   manage gateway secrets (real values never leave it)
  piso expose <port> [--name n]  reverse-proxy a worker port as https://n.piso.local
  piso logs [--follow]   tail the gateway request log (SSE when --follow)
  piso dashboard         open the gateway web UI in the browser
`)
}

// ---- up / down ----

func cmdUp(args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	proj, err := pisoconfig.Resolve(abs)
	if err != nil {
		return err
	}
	fmt.Printf("piso: project %q (slug %s)\n", proj.Dir, proj.Slug)

	// 1. gateway (shared, one per machine). Compose will not flip Internal on
	// an already-created piso_vpc, so tear down a leaky one first.
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
	if err := runEnv("docker", []string{"compose", "-f", gatewayFile, "-p", "piso", "up", "-d", "--build", "-t", "0"},
		composeEnv); err != nil {
		return fmt.Errorf("gateway up: %w", err)
	}
	if err := dockernet.AssertInternal(); err != nil {
		return err
	}
	// 2. worker (per-project)
	workerFile, err := pisoconfig.WriteWorkerCompose(proj)
	if err != nil {
		return err
	}
	if err := runEnv("docker", []string{"compose", "-f", workerFile, "-p", "piso-" + proj.Slug, "up", "-d", "--build", "-t", "0"}, composeEnv); err != nil {
		return fmt.Errorf("worker up: %w", err)
	}
	// 3. wait for the gateway control plane
	gw := pisoconfig.GatewayURL()
	for i := 0; i < 30; i++ {
		if healthy(gw) {
			fmt.Printf("piso: gateway live at %s — dashboard: %s\n", gw, gw)
			fmt.Printf("piso: worker %s ready. Run `piso attach`.\n", proj.WorkerName())
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("gateway did not become healthy within 30s (log: %s)", gatewayFile)
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
	cmdArgs := []string{"exec", "-it", proj.WorkerName()}
	if len(args) > 0 && args[0] == "--shell" {
		cmdArgs = append(cmdArgs, "/bin/bash")
	} else {
		cmdArgs = append(cmdArgs, "pi", "-r") // resume the pi session browser
	}
	c := exec.Command("docker", cmdArgs...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// ---- status / secrets / expose / logs / dashboard ----

func cmdStatus(args []string) error {
	gw := pisoconfig.GatewayURL()
	if healthy(gw) {
		fmt.Println("gateway: live (" + gw + ")")
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
	fmt.Printf("piso: https://%s.piso.local → %s:%d\n", name, worker, port)
	return nil
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
	gw := pisoconfig.GatewayURL()
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

// ---- shared helpers ----

// dockerComposeEnv is required for compose interpolation of ${PISO_DATA}
// (gateway bind-mount) and for BuildKit on image builds.
func dockerComposeEnv() (map[string]string, error) {
	data, err := pisoconfig.DataDir()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"DOCKER_BUILDKIT": "1",
		"PISO_DATA":       data,
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
	fmt.Printf("%s %-11s %-3d %-5s %s://%s%s  %s%s\n",
		r.Ts.Format("15:04:05"), r.Action, r.Status, r.Method,
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if env != nil {
		c.Env = append(os.Environ(), envPairs(env)...)
	}
	return c.Run()
}

func envPairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
