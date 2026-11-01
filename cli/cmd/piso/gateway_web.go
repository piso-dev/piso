package main

import (
	"fmt"
	"net"
	"os"
	"strings"

	"piso/cli/internal/pisoconfig"
	"piso/internal/technocore"
)

const gatewayPublicNetwork = "piso_public"

// reconcileStandaloneWeb trusts only the dedicated proxy bridge. Never infer
// trust from arbitrary interfaces or from the worker network, piso_vpc.
func reconcileStandaloneWeb(id technocore.Identity) error {
	if id.PlacementOrLocal() != technocore.PlacementStandalone {
		return nil
	}
	names, err := dockerOutputLite("network", "ls", "--filter", "name=^"+gatewayPublicNetwork+"$", "--format", "{{.Name}}")
	if err != nil {
		return fmt.Errorf("discover gateway proxy network: %w", err)
	}
	if strings.TrimSpace(names) == "" {
		// Let Compose create its own network with the correct ownership labels.
		// Until trust is saved, proxy requests remain denied by the peer gate.
		if err := startGateway(false); err != nil {
			return err
		}
	}
	raw, err := dockerOutputLite("network", "inspect", gatewayPublicNetwork, "--format", "{{range .IPAM.Config}}{{println .Subnet}}{{end}}")
	if err != nil {
		return fmt.Errorf("inspect gateway proxy network: %w", err)
	}
	cidrs, err := proxyNetworkCIDRs(raw)
	if err != nil {
		return err
	}
	web, err := pisoconfig.LoadGatewayWebSettings()
	if err != nil {
		return err
	}
	// Preserve an explicit saved trust policy; only fill the missing default.
	if web.TrustedPeerCIDRs == "" {
		web.TrustedPeerCIDRs = cidrs
	}
	if value := strings.TrimSpace(os.Getenv("PISO_TRUSTED_PEER_CIDRS")); value != "" {
		web.TrustedPeerCIDRs = value
	}
	if value := strings.TrimSpace(os.Getenv("PISO_DASHBOARD_HOST")); value != "" {
		web.DashboardHost = value
	}
	return pisoconfig.SaveGatewayWebSettings(web)
}

func proxyNetworkCIDRs(raw string) (string, error) {
	var cidrs []string
	for _, item := range strings.Fields(raw) {
		_, network, err := net.ParseCIDR(item)
		if err != nil {
			return "", fmt.Errorf("invalid proxy network subnet %q: %w", item, err)
		}
		cidrs = append(cidrs, network.String())
	}
	if len(cidrs) == 0 {
		return "", fmt.Errorf("%s has no subnet; cannot configure proxy trust", gatewayPublicNetwork)
	}
	return strings.Join(cidrs, ","), nil
}
