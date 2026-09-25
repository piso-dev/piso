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
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"piso/cli/internal/pisoconfig"
	"piso/internal/technocore"
)

const defaultTechnocoreServer = "https://server.com"

func cmdGateway(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: piso gateway launch|status")
	}
	switch args[0] {
	case "launch":
		return cmdGatewayLaunch(args[1:])
	case "status":
		return cmdGatewayStatus()
	default:
		return errors.New("usage: piso gateway launch|status")
	}
}

func cmdGatewayLaunch(args []string) error {
	fs := flag.NewFlagSet("gateway launch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "gateway display name")
	server := fs.String("server", strings.TrimSpace(os.Getenv("PISO_SERVER_URL")), "technocore server URL")
	noQR := fs.Bool("no-qr", false, "print the approve URL without a terminal QR")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: piso gateway launch [--name name] [--server url] [--no-qr]")
	}
	if *server == "" {
		*server = defaultTechnocoreServer
	}
	if *name == "" {
		host, _ := os.Hostname()
		*name = host
	}

	path, err := identityPath()
	if err != nil {
		return err
	}
	id, err := technocore.LoadOrCreate(path, *server, *name)
	if err != nil {
		return err
	}
	if id.Status == technocore.StatusActive && id.PrincipalID != "" {
		fmt.Printf("piso: gateway already enrolled as %s (%s)\n", id.Name, id.PrincipalID)
		fmt.Println("piso: the local gateway holds a WebSocket to the server while it runs")
		return nil
	}

	if id.PairingID == "" || id.Status != technocore.StatusPending {
		if err := startPairing(&id, path); err != nil {
			return err
		}
	}

	approveURL := fmt.Sprintf("%s/pair/%s/", id.ServerURL, id.PairingID)
	fmt.Printf("piso: fingerprint %s\n", id.Fingerprint())
	fmt.Printf("piso: approve on your phone (logged in if needed):\n  %s\n", approveURL)
	if !*noQR {
		if err := printApproveQR(approveURL); err != nil {
			fmt.Fprintf(os.Stderr, "piso: qr: %v\n", err)
		}
	}
	if !healthy(pisoconfig.ControlAPIURL()) {
		fmt.Println("piso: local gateway is not up; run `piso up` so it can connect after approval")
	}
	fmt.Println("piso: waiting for approval...")

	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		status, principal, err := pollPairing(id)
		if err != nil {
			return err
		}
		switch status {
		case "approved":
			id.Status = technocore.StatusActive
			id.PrincipalID = principal
			if err := technocore.Save(path, id); err != nil {
				return err
			}
			fmt.Printf("piso: gateway enrolled (%s)\n", principal)
			fmt.Println("piso: leave the gateway running (`piso up`); it will connect and sync secrets")
			return nil
		case "rejected":
			return errors.New("pairing rejected")
		case "expired":
			return errors.New("pairing expired; rerun `piso gateway launch`")
		}
		time.Sleep(2 * time.Second)
	}
	return errors.New("timed out waiting for approval")
}

func cmdGatewayStatus() error {
	path, err := identityPath()
	if err != nil {
		return err
	}
	id, err := technocore.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("technocore: not enrolled (run `piso gateway launch`)")
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

func printApproveQR(url string) error {
	q, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return err
	}
	fmt.Print(q.ToSmallString(false))
	return nil
}

func identityPath() (string, error) {
	dir, err := pisoconfig.DataDir()
	if err != nil {
		return "", err
	}
	return technocore.Path(dir), nil
}

func startPairing(id *technocore.Identity, path string) error {
	body, _ := json.Marshal(map[string]any{
		"kind":               "gateway",
		"device_pubkey":      id.SigningPublicKey,
		"encryption_pubkey":  id.EncryptionPublicKey,
		"name":               id.Name,
		"capabilities":       []string{"credential-relay"},
	})
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

func pollPairing(id technocore.Identity) (status, principal string, err error) {
	u := fmt.Sprintf("%s/api/v1/pairing-requests/%s", id.ServerURL, id.PairingID)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set(technocore.HeaderPollToken, id.PollToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 200 {
		return "", "", fmt.Errorf("poll HTTP %d: %s", res.StatusCode, raw)
	}
	var out struct {
		Status      string `json:"status"`
		PrincipalID string `json:"principal_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", err
	}
	return out.Status, out.PrincipalID, nil
}
