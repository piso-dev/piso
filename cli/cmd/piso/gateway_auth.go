package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"piso/cli/internal/pisoconfig"
	"piso/internal/authlink"
	"piso/internal/hostauth"
	"piso/internal/nostrkey"
	"piso/internal/technocore"
)

// Dashboard authentication from the host's side.
//
// Enrollment is the only way a credential can be created, and it is minted here
// rather than by the gateway at first boot: root on the host is the
// authentication event, and no secret is ever printed into `docker logs`.
// Everything else (status, revoking a credential, signing out everywhere) is an
// administrative call signed with the same identity key, so the endpoints stay
// closed to anonymous callers.

func cmdGatewayAuth(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: piso gateway auth enroll|add-nostr|status|logout-all|remove <id>")
	}
	switch args[0] {
	case "enroll":
		return cmdGatewayAuthEnroll(args[1:])
	case "add-nostr":
		return cmdGatewayAuthAddNostr(args[1:])
	case "status":
		return cmdGatewayAuthStatus()
	case "logout-all":
		return cmdGatewayAuthLogoutAll()
	case "remove":
		return cmdGatewayAuthRemove(args[1:])
	default:
		return errors.New("usage: piso gateway auth enroll|add-nostr|status|logout-all|remove <id>")
	}
}

// cmdGatewayAuthEnroll mints a single-use enrollment link.
func cmdGatewayAuthEnroll(args []string) error {
	fs := flag.NewFlagSet("gateway auth enroll", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	host := fs.String("host", "", "dashboard hostname (default: PISO_DASHBOARD_HOST, else the public URL's host)")
	ttl := fs.Duration("ttl", authlink.TTL, "how long the link stays valid")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := currentIdentity()
	if err != nil {
		return err
	}
	base, err := dashboardBase(id, *host)
	if err != nil {
		return err
	}
	if *ttl <= 0 || *ttl > authlink.MaxLifetime {
		return fmt.Errorf("--ttl must be between 0 and %s", authlink.MaxLifetime)
	}
	nonce, err := authlink.NewNonce()
	if err != nil {
		return err
	}
	exp := time.Now().Add(*ttl).Unix()
	sig, err := authlink.Sign(id.SigningPrivateKey, nonce, exp)
	if err != nil {
		return err
	}
	link := authlink.URL(base, nonce, sig, exp)
	// The link is signed by *this* host's identity key, and a gateway verifies it
	// against its own — so the link only ever works on the gateway that holds the
	// matching key. Say which one that is: a worker host has a different identity
	// in ~/.piso, and a link minted there is refused by every gateway.
	name := id.Name
	if name == "" {
		name = "(unnamed)"
	}
	fmt.Printf("enrollment link (single use, %s):\n\n  %s\n\n", *ttl, link)
	fmt.Printf("for gateway %s (%s)\n\n", name, id.Fingerprint())
	if id.Status != technocore.StatusActive {
		status := id.Status
		if status == "" {
			status = "new"
		}
		fmt.Fprintf(os.Stderr, "warning: this identity is %q, not an active gateway.\n", status)
		fmt.Fprintln(os.Stderr, "         The link is signed by this host, so only a gateway holding this")
		fmt.Fprintln(os.Stderr, "         identity accepts it. Run this on the gateway host, or with its PISO_DATA.")
		fmt.Fprintln(os.Stderr)
	}
	fmt.Println("Open it in a browser on the dashboard host's network and register a")
	fmt.Println("passkey or a nostr signer. Root on this host is what authorises it.")
	return nil
}

// cmdGatewayAuthAddNostr adds nostr public keys to *this* gateway from the host.
//
// This is the fleet path: enrollment needs a browser window per gateway, which
// does not scale past two hosts and quietly encourages one shared key pasted
// everywhere. Root on the gateway already implies the ability to do anything
// here, so the signer does not have to be present — only the host key does.
//
// It grants one gateway, not the fleet. Run it on each gateway you want the key
// to work on; a key is portable, an authorization is not.
func cmdGatewayAuthAddNostr(args []string) error {
	fs := flag.NewFlagSet("gateway auth add-nostr", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	label := fs.String("label", "", "label to show in `piso gateway auth status` (default: \"added from the host\")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	keys := fs.Args()
	if len(keys) == 0 {
		return errors.New("usage: piso gateway auth add-nostr [--label L] <pubkey|npub>...")
	}
	// Normalise before sending so a typo is refused here, with a readable
	// message, instead of travelling to the gateway to come back as a 400.
	normalized := make([]string, 0, len(keys))
	for _, k := range keys {
		pub, err := nostrkey.Normalize(k)
		if err != nil {
			return err
		}
		normalized = append(normalized, pub)
	}
	note := strings.TrimSpace(*label)
	if note == "" {
		note = "added from the host"
	}
	for _, pub := range normalized {
		var out struct {
			ID     string `json:"id"`
			Pubkey string `json:"pubkey"`
		}
		if err := gatewayAuthCall(http.MethodPost, "/api/v1/auth/nostr-keys", map[string]string{
			"pubkey": pub,
			"label":  note,
		}, &out); err != nil {
			return err
		}
		fmt.Printf("added nostr key %s  id=%s\n", out.Pubkey, out.ID)
	}
	fmt.Println()
	fmt.Println("Sign in at the dashboard with that key — the gateway mints a challenge and")
	fmt.Println("checks the signature, so no browser registration step is needed.")
	fmt.Println("To undo: piso gateway auth remove <id>")
	return nil
}

// dashboardBase resolves the dashboard origin the link should point at.
func dashboardBase(id technocore.Identity, flagHost string) (string, error) {
	host := strings.TrimSpace(flagHost)
	if host == "" {
		host = strings.TrimSpace(os.Getenv("PISO_DASHBOARD_HOST"))
	}
	if host == "" {
		host = strings.TrimSpace(os.Getenv("GATEWAY_DOMAIN"))
	}
	if host != "" {
		if strings.Contains(host, "://") {
			return strings.TrimRight(host, "/"), nil
		}
		return "https://" + strings.TrimRight(host, "/"), nil
	}
	if id.PublicURL == "" {
		return "", errors.New("cannot tell where the dashboard is: pass --host dashboard.example.com")
	}
	u, err := url.Parse(id.PublicURL)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("public url %q is not parseable: pass --host", id.PublicURL)
	}
	return "https://" + u.Hostname(), nil
}

// gatewayAuthCall performs a host-signed administrative request. A non-nil body
// is sent as JSON and is covered by the signature (hostauth v2).
func gatewayAuthCall(method, path string, body, out any) error {
	id, err := currentIdentity()
	if err != nil {
		return err
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	gw := pisoconfig.GatewayURL()
	req, err := http.NewRequest(method, gw+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := hostauth.Apply(req, id.SigningPrivateKey, time.Now()); err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("gateway unreachable at %s: %w", gw, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &e) == nil && e.Error != "" {
			return fmt.Errorf("gateway: %s", e.Error)
		}
		return fmt.Errorf("gateway: HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

type authStateOut struct {
	Required    bool   `json:"required"`
	Reason      string `json:"reason"`
	Host        string `json:"host"`
	Epoch       int    `json:"epoch"`
	Credentials []struct {
		ID        string    `json:"id"`
		Kind      string    `json:"kind"`
		Label     string    `json:"label"`
		Pubkey    string    `json:"pubkey"`
		CreatedAt time.Time `json:"createdAt"`
		LastUsed  time.Time `json:"lastUsed"`
	} `json:"credentials"`
}

func cmdGatewayAuthStatus() error {
	var st authStateOut
	if err := gatewayAuthCall(http.MethodGet, "/api/v1/auth/state", nil, &st); err != nil {
		return err
	}
	switch {
	case st.Required && st.Reason != "":
		fmt.Printf("dashboard login: required (%s)\n", st.Reason)
	case st.Required:
		fmt.Println("dashboard login: required")
	default:
		fmt.Println("dashboard login: not required on this host (control plane is not reachable off-host)")
		fmt.Println("                 force it with PISO_DASHBOARD_AUTH=on")
	}
	if st.Host != "" {
		fmt.Printf("dashboard host:  https://%s\n", st.Host)
	}
	fmt.Printf("session epoch:   %d (bump with `piso gateway auth logout-all`)\n", st.Epoch)
	if len(st.Credentials) == 0 {
		fmt.Println("credentials:     none — run `piso gateway auth enroll` to add one")
		return nil
	}
	fmt.Printf("credentials:     %d\n", len(st.Credentials))
	for _, c := range st.Credentials {
		used := "never used"
		if !c.LastUsed.IsZero() {
			used = "last used " + c.LastUsed.Format(time.RFC3339)
		}
		label := c.Label
		if label == "" {
			label = "(unlabelled)"
		}
		fmt.Printf("  %-8s %-12s %-16s %s  id=%s\n", c.Kind, label, used, c.CreatedAt.Format("2006-01-02"), c.ID)
		if c.Pubkey != "" {
			fmt.Printf("           pubkey %s\n", c.Pubkey)
		}
	}
	return nil
}

func cmdGatewayAuthLogoutAll() error {
	var out struct {
		Epoch int `json:"epoch"`
	}
	if err := gatewayAuthCall(http.MethodPost, "/api/v1/auth/logout-all", nil, &out); err != nil {
		return err
	}
	fmt.Printf("all dashboard sessions revoked (epoch %d)\n", out.Epoch)
	return nil
}

func cmdGatewayAuthRemove(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: piso gateway auth remove <credential id>")
	}
	path := "/api/v1/auth/credentials/" + url.PathEscape(args[0])
	if err := gatewayAuthCall(http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	fmt.Printf("credential %s removed\n", args[0])
	fmt.Println("existing sessions for it stop working on the next request")
	return nil
}

// currentIdentity loads this host's gateway identity.
func currentIdentity() (technocore.Identity, error) {
	path, err := identityPath()
	if err != nil {
		return technocore.Identity{}, err
	}
	id, err := technocore.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return technocore.Identity{}, errors.New("not enrolled yet: run `piso gateway launch` first")
		}
		return technocore.Identity{}, err
	}
	return id, nil
}
