package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
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

func resolvePlacementFlag(standalone bool, placement string) (string, bool, error) {
	p := strings.TrimSpace(placement)
	if standalone {
		if p != "" && strings.ToLower(p) != technocore.PlacementStandalone {
			return "", false, errors.New("--standalone conflicts with --placement " + placement)
		}
		return technocore.PlacementStandalone, true, nil
	}
	if p == "" {
		return "", false, nil
	}
	norm, err := technocore.NormalizePlacement(p)
	if err != nil {
		return "", false, err
	}
	return norm, true, nil
}

func cmdGatewayLaunch(args []string) error {
	fs := flag.NewFlagSet("gateway launch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "gateway display name")
	server := fs.String("server", strings.TrimSpace(os.Getenv("PISO_SERVER_URL")), "technocore server URL")
	noQR := fs.Bool("no-qr", false, "print the approve URL without a terminal QR")
	yes := fs.Bool("yes", false, "confirm the enrollment without opening the dashboard (headless runners)")
	standalone := fs.Bool("standalone", false, "always-on gateway; eligible to host launched workers")
	placement := fs.String("placement", "", "local or standalone (default local; --standalone is an alias)")
	publicURL := fs.String("url", "", "public HTTPS URL for remote workers (implies --standalone), e.g. https://gw.example.com:8443")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: piso gateway launch [--name name] [--server url] [--no-qr] [--yes] [--standalone] [--url https://host:8443]")
	}
	*publicURL = strings.TrimRight(strings.TrimSpace(*publicURL), "/")
	if *publicURL != "" {
		*standalone = true
	}
	wantPlacement, placementSet, err := resolvePlacementFlag(*standalone, *placement)
	if err != nil {
		return err
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
	if placementSet {
		id.Placement = wantPlacement
	}
	if *publicURL != "" {
		id.PublicURL = *publicURL
		id.Placement = technocore.PlacementStandalone
		placementSet = true
	}
	if placementSet || *publicURL != "" {
		if err := technocore.Save(path, id); err != nil {
			return err
		}
	}
	if id.Status == technocore.StatusActive && id.PrincipalID != "" {
		fmt.Printf("piso: gateway already enrolled as %s (%s)\n", id.Name, id.PrincipalID)
		fmt.Printf("piso: placement %s\n", id.PlacementOrLocal())
		if id.PublicURL != "" {
			fmt.Printf("piso: public url %s\n", id.PublicURL)
		} else if id.PlacementOrLocal() == technocore.PlacementStandalone {
			fmt.Println("piso: no --url; remote workers will reverse-tunnel through the coordinator")
		}
		fmt.Println("piso: the local gateway holds a WebSocket to the server while it runs")
		if placementSet {
			fmt.Println("piso: placement will be advertised on the next heartbeat")
			fmt.Println("piso: restart the gateway (`piso up`) so it listens for remote workers")
		}
		return nil
	}
	if id.Placement == "" {
		id.Placement = technocore.PlacementLocal
	}

	if id.PairingID == "" || id.Status != technocore.StatusPending {
		updated, err := technocore.StartPairing(path, id)
		if err != nil {
			return err
		}
		id = updated
	}

	approveURL := technocore.PairURL(id)
	fmt.Printf("piso: fingerprint %s\n", id.Fingerprint())
	fmt.Printf("piso: placement %s\n", id.PlacementOrLocal())
	if id.PublicURL != "" {
		fmt.Printf("piso: public url %s\n", id.PublicURL)
	}
	fmt.Printf("piso: approve on your phone (logged in if needed):\n  %s\n", approveURL)
	if !*noQR {
		if err := printApproveQR(approveURL); err != nil {
			fmt.Fprintf(os.Stderr, "piso: qr: %v\n", err)
		}
	}
	if !healthy(pisoconfig.ControlAPIURL()) {
		fmt.Println("piso: local gateway is not up; run `piso up` so it can connect after approval")
	}
	fmt.Println("piso: waiting for approval on the phone...")

	deadline := time.Now().Add(15 * time.Minute)
	confirmPrinted := false
	for time.Now().Before(deadline) {
		state, err := technocore.PollPairing(id)
		if err != nil {
			if errors.Is(err, technocore.ErrPollToken) {
				return errors.New("pairing was cancelled on the server; rerun `piso gateway launch`")
			}
			return err
		}
		switch state.Status {
		case technocore.PairStatusAwaitingGateway:
			// The phone only recorded intent. Enrollment needs a second tap on
			// the machine that will hold the keys.
			if *yes {
				principal, err := technocore.ConfirmPairing(id)
				if err != nil {
					return err
				}
				if err := enrollLocal(path, id, principal); err != nil {
					return err
				}
				fmt.Printf("piso: gateway enrolled (%s)\n", principal)
				fmt.Println("piso: leave the gateway running; it will connect and sync secrets")
				return nil
			}
			if !confirmPrinted {
				confirmPrinted = true
				who := state.ApprovedBy
				if who == "" {
					who = "someone"
				}
				fmt.Printf("piso: %s approved this gateway on their phone.\n", who)
				fmt.Printf("piso: confirm it here: %s/#/live\n", pisoconfig.DashboardURL())
				fmt.Println("piso: or rerun with --yes to confirm from this terminal")
			}
		case technocore.PairStatusApproved:
			if err := enrollLocal(path, id, state.PrincipalID); err != nil {
				return err
			}
			fmt.Printf("piso: gateway enrolled (%s)\n", state.PrincipalID)
			fmt.Println("piso: leave the gateway running; it will connect and sync secrets")
			return nil
		case technocore.PairStatusRejected:
			return errors.New("pairing rejected")
		case technocore.PairStatusExpired:
			return errors.New("pairing expired; rerun `piso gateway launch`")
		}
		time.Sleep(2 * time.Second)
	}
	return errors.New("timed out waiting for approval")
}

func enrollLocal(path string, id technocore.Identity, principal string) error {
	_, err := technocore.Update(path, func(cur *technocore.Identity) error {
		cur.Status = technocore.StatusActive
		cur.PrincipalID = principal
		cur.PairPhase = ""
		cur.PairApprover = ""
		cur.PairAccount = ""
		return nil
	})
	return err
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
	status := id.Status
	if status == technocore.StatusNew || status == "" {
		status = "not enrolled"
	}
	fmt.Printf("status:      %s\n", status)
	fmt.Printf("placement:   %s\n", id.PlacementOrLocal())
	if id.PublicURL != "" {
		fmt.Printf("public url:  %s\n", id.PublicURL)
	}
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
