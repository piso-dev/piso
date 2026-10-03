package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"piso/cli/internal/dockernet"
	"piso/cli/internal/pisoconfig"
	"piso/internal/technocore"
)

func cmdWorker(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: piso worker launch|status")
	}
	switch args[0] {
	case "launch":
		return cmdWorkerLaunch(args[1:])
	case "status":
		return cmdWorkerStatus(args[1:])
	default:
		return errors.New("usage: piso worker launch|status")
	}
}

func cmdWorkerLaunch(args []string) error {
	fs := flag.NewFlagSet("worker launch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "worker slug")
	server := fs.String("server", strings.TrimSpace(os.Getenv("PISO_SERVER_URL")), "technocore server URL")
	noQR := fs.Bool("no-qr", false, "print the approve URL without a terminal QR")
	if err := fs.Parse(args); err != nil {
		return errors.New("usage: piso worker launch [dir] [--name n] [--server url] [--no-qr]")
	}
	dir := "."
	if rest := fs.Args(); len(rest) > 0 {
		dir = rest[0]
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	local, localErr := loadLocalGatewayIdentity()
	if err := errIfStandaloneGatewayHost(local, localErr); err != nil {
		return err
	}
	if *server == "" {
		if localErr == nil {
			*server = local.ServerURL
		}
		if *server == "" {
			*server = defaultTechnocoreServer
		}
	}

	proj, err := pisoconfig.Resolve(abs)
	if err != nil {
		return err
	}
	slug, err := chooseWorkerSlug(proj.Dir, *name)
	if err != nil {
		return err
	}
	proj.Slug = slug

	wpath, err := workerIdentityPath(slug)
	if err != nil {
		return err
	}
	wid, err := technocore.LoadOrCreate(wpath, *server, slug)
	if err != nil {
		return err
	}

	var asg workerAssignment
	if wid.Status != technocore.StatusActive || wid.PrincipalID == "" {
		asg, err = pairWorker(&wid, wpath, technocore.Identity{}, *noQR)
		if err != nil {
			return err
		}
	} else {
		fmt.Printf("piso: worker already enrolled as %s (%s)\n", wid.Name, wid.PrincipalID)
		asg, _ = loadWorkerAssignment(slug)
		if asg.Endpoint == "" {
			asg, _ = pollWorkerAssignment(wid)
		}
	}
	fmt.Println("piso: starting a standalone worker; egress is the assigned gateway (never this machine)")
	return startRemoteWorker(proj, asg)
}

// errIfStandaloneGatewayHost keeps the vault off the worker's machine.
// A launched worker is a cloud ship: it may be root on its box, so it must
// not share a host with a standalone gateway.
func errIfStandaloneGatewayHost(id technocore.Identity, loadErr error) error {
	if loadErr != nil {
		return nil
	}
	if id.Status == technocore.StatusActive && id.PlacementOrLocal() == technocore.PlacementStandalone {
		return errors.New("this machine is a standalone gateway. launched workers must run on a different machine so they can have root without owning the vault")
	}
	return nil
}

func cmdWorkerStatus(args []string) error {
	fs := flag.NewFlagSet("worker status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "worker slug")
	if err := fs.Parse(args); err != nil {
		return errors.New("usage: piso worker status [dir] [--name n]")
	}
	dir := "."
	if rest := fs.Args(); len(rest) > 0 {
		dir = rest[0]
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	proj, err := pisoconfig.Resolve(abs)
	if err != nil {
		return err
	}
	slug := pisoconfig.NormalizeWorkerName(*name)
	if slug == "" {
		slug = existingSlugForDir(proj.Dir)
	}
	if slug == "" {
		slug, _ = pisoconfig.LoadProjectSlug(proj.Dir)
	}
	if slug == "" {
		slug = pisoconfig.DefaultSlug(proj.Dir)
	}
	path, err := workerIdentityPath(slug)
	if err != nil {
		return err
	}
	id, err := technocore.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("worker %s: not enrolled (run `piso worker launch`)\n", slug)
			return nil
		}
		return err
	}
	fmt.Printf("file:        %s\n", path)
	fmt.Printf("server:      %s\n", id.ServerURL)
	fmt.Printf("name:        %s\n", id.Name)
	fmt.Printf("status:      %s\n", id.Status)
	fmt.Printf("fingerprint: %s\n", id.Fingerprint())
	if id.PrincipalID != "" {
		fmt.Printf("principal:   %s\n", id.PrincipalID)
	}
	return nil
}

func loadLocalGatewayIdentity() (technocore.Identity, error) {
	path, err := identityPath()
	if err != nil {
		return technocore.Identity{}, err
	}
	id, err := technocore.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return technocore.Identity{}, errors.New("not enrolled (run `piso gateway launch --standalone`)")
		}
		return technocore.Identity{}, err
	}
	return id, nil
}

func workerIdentityPath(slug string) (string, error) {
	dir, err := pisoconfig.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "technocore", "workers", slug, "identity.json"), nil
}

func workerAssignmentPath(slug string) (string, error) {
	dir, err := pisoconfig.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "technocore", "workers", slug, "assignment.json"), nil
}

func saveWorkerAssignment(slug string, asg workerAssignment) error {
	path, err := workerAssignmentPath(slug)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(asg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func loadWorkerAssignment(slug string) (workerAssignment, error) {
	var zero workerAssignment
	path, err := workerAssignmentPath(slug)
	if err != nil {
		return zero, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return zero, err
	}
	if err := json.Unmarshal(raw, &zero); err != nil {
		return workerAssignment{}, err
	}
	return zero, nil
}

type workerAssignment struct {
	Status      string
	PrincipalID string
	GatewayID   string
	Endpoint    string
	Mode        string
	CAPem       string
	Token       string
}

func pairWorker(id *technocore.Identity, path string, gw technocore.Identity, noQR bool) (workerAssignment, error) {
	var zero workerAssignment
	if id.PairingID == "" || id.Status != technocore.StatusPending {
		if err := startWorkerPairing(id, path, gw.PrincipalID); err != nil {
			return zero, err
		}
	}
	approveURL := fmt.Sprintf("%s/pair/%s/", id.ServerURL, id.PairingID)
	fmt.Printf("piso: worker fingerprint %s\n", id.Fingerprint())
	if gw.PrincipalID != "" {
		fmt.Printf("piso: will attach to standalone gateway %s (%s)\n", gw.Name, gw.PrincipalID)
	} else {
		fmt.Println("piso: coordinator will assign an available standalone gateway")
	}
	fmt.Printf("piso: approve on your phone (logged in if needed):\n  %s\n", approveURL)
	if !noQR {
		if err := printApproveQR(approveURL); err != nil {
			fmt.Fprintf(os.Stderr, "piso: qr: %v\n", err)
		}
	}
	fmt.Println("piso: waiting for approval...")

	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		asg, err := pollWorkerAssignment(*id)
		if err != nil {
			return zero, err
		}
		switch asg.Status {
		case "approved":
			id.Status = technocore.StatusActive
			id.PrincipalID = asg.PrincipalID
			if err := technocore.Save(path, *id); err != nil {
				return zero, err
			}
			fmt.Printf("piso: worker enrolled (%s) via %s %s\n", asg.PrincipalID, asg.Mode, asg.Endpoint)
			_ = saveWorkerAssignment(id.Name, asg)
			return asg, nil
		case "rejected":
			return zero, errors.New("pairing rejected")
		case "expired":
			return zero, errors.New("pairing expired; rerun `piso worker launch`")
		}
		time.Sleep(2 * time.Second)
	}
	return zero, errors.New("timed out waiting for approval")
}

func pollWorkerAssignment(id technocore.Identity) (workerAssignment, error) {
	var zero workerAssignment
	u := fmt.Sprintf("%s/api/v1/pairing-requests/%s", id.ServerURL, id.PairingID)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return zero, err
	}
	req.Header.Set(technocore.HeaderPollToken, id.PollToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 200 {
		return zero, fmt.Errorf("poll HTTP %d: %s", res.StatusCode, raw)
	}
	var out struct {
		Status           string `json:"status"`
		PrincipalID      string `json:"principal_id"`
		GatewayID        string `json:"gateway_id"`
		GatewayEndpoint  string `json:"gateway_endpoint"`
		GatewayMode      string `json:"gateway_mode"`
		CAPem            string `json:"ca_pem"`
		WorkerToken      string `json:"worker_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, err
	}
	return workerAssignment{
		Status:      out.Status,
		PrincipalID: out.PrincipalID,
		GatewayID:   out.GatewayID,
		Endpoint:    out.GatewayEndpoint,
		Mode:        out.GatewayMode,
		CAPem:       out.CAPem,
		Token:       out.WorkerToken,
	}, nil
}

func startRemoteWorker(proj pisoconfig.Project, asg workerAssignment) error {
	if asg.Endpoint == "" {
		return errors.New("assigned gateway has no worker endpoint yet (wait for it to heartbeat, then rerun piso worker launch)")
	}
	if err := dockernet.AssertLinuxEngine(); err != nil {
		return err
	}
	data, err := pisoconfig.DataDir()
	if err != nil {
		return err
	}
	slugDir := filepath.Join(data, "workers", proj.Slug)
	if err := os.MkdirAll(slugDir, 0o700); err != nil {
		return err
	}
	if asg.CAPem != "" {
		if err := os.WriteFile(filepath.Join(slugDir, "ca.crt"), []byte(asg.CAPem), 0o644); err != nil {
			return err
		}
	}
	if asg.Token != "" {
		if err := os.WriteFile(filepath.Join(slugDir, "token"), []byte(asg.Token+"\n"), 0o600); err != nil {
			return err
		}
	}
	if err := pisoconfig.EnsureWorkerPlaceholdersEnv(proj.Slug); err != nil {
		return err
	}
	file, err := pisoconfig.WriteRemoteWorkerCompose(proj, asg.Endpoint, asg.Token)
	if err != nil {
		return err
	}
	env, err := dockerComposeEnv()
	if err != nil {
		return err
	}
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
	upArgs := []string{"compose", "-f", file, "-p", "piso-" + proj.Slug, "up", "-d", "-t", "0"}
	if err := runEnv("docker", upArgs, env); err != nil {
		return fmt.Errorf("worker up: %w", err)
	}
	if err := waitWorkerReady(proj.WorkerName()); err != nil {
		return err
	}
	fmt.Printf("piso: remote worker %s ready via %s (%s). Run `piso attach`.\n", proj.WorkerName(), asg.Mode, asg.Endpoint)
	return nil
}

func startWorkerPairing(id *technocore.Identity, path, gatewayPrincipalID string) error {
	bodyMap := map[string]any{
		"kind":              "worker",
		"device_pubkey":     id.SigningPublicKey,
		"encryption_pubkey": id.EncryptionPublicKey,
		"name":              id.Name,
		"capabilities":      []string{"hull:cloud_ship"},
	}
	if gatewayPrincipalID != "" {
		bodyMap["gateway_id"] = gatewayPrincipalID
	}
	body, _ := json.Marshal(bodyMap)
	res, err := http.Post(id.ServerURL+"/api/v1/pairing-requests", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pairing: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 201 {
		return fmt.Errorf("pairing HTTP %d: %s", res.StatusCode, raw)
	}
	var out struct {
		ID        string `json:"id"`
		PollToken string `json:"poll_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	id.PairingID = out.ID
	id.PollToken = out.PollToken
	id.Status = technocore.StatusPending
	return technocore.Save(path, *id)
}
