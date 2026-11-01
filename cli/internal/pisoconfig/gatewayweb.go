package pisoconfig

import (
	"os"
	"strings"
)

// GatewayWebSettings describes the human-facing endpoint, independently of
// the identity's PublicURL (the worker TLS endpoint).
type GatewayWebSettings struct {
	DashboardHost    string `json:"dashboardHost,omitempty"`
	TrustedPeerCIDRs string `json:"trustedPeerCIDRs,omitempty"`
	PublicSubnet     string `json:"publicSubnet,omitempty"`
}

func LoadGatewayWebSettings() (GatewayWebSettings, error) {
	s, err := loadHostSettings()
	if os.IsNotExist(err) {
		return GatewayWebSettings{}, nil
	}
	return s.GatewayWeb, err
}

// SaveGatewayWebSettings preserves the monitor and unrelated host settings.
func SaveGatewayWebSettings(web GatewayWebSettings) error {
	s, err := loadHostSettings()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s.GatewayWeb = web
	return saveHostSettings(s)
}

// GatewayWebEnv uses explicit per-invocation overrides, then durable settings.
// With neither, local installs keep their existing empty defaults.
func GatewayWebEnv() (GatewayWebSettings, error) {
	web, err := LoadGatewayWebSettings()
	if err != nil {
		return web, err
	}
	if v, ok := os.LookupEnv("PISO_DASHBOARD_HOST"); ok {
		web.DashboardHost = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("PISO_TRUSTED_PEER_CIDRS"); ok {
		web.TrustedPeerCIDRs = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("PISO_PUBLIC_SUBNET"); ok {
		web.PublicSubnet = strings.TrimSpace(v)
	}
	return web, nil
}
