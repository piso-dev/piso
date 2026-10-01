package githubapp

import "encoding/json"

// Manifest is the GitHub App Manifest payload (permissions + callback URLs).
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	RedirectURL        string            `json:"redirect_url"`
	SetupURL           string            `json:"setup_url,omitempty"`
	CallbackURLs       []string          `json:"callback_urls,omitempty"`
	Public             bool              `json:"public"`
	DefaultEvents      []string          `json:"default_events"`
	DefaultPermissions map[string]string `json:"default_permissions"`
}

// NewManifest builds the fixed piso technocore app template.
func NewManifest(name, homepage, redirect, setup string) Manifest {
	if name == "" {
		name = "piso-technocore"
	}
	return Manifest{
		Name:          name,
		URL:           homepage,
		RedirectURL:   redirect,
		SetupURL:      setup,
		CallbackURLs:  []string{redirect},
		Public:        false,
		DefaultEvents: []string{},
		DefaultPermissions: map[string]string{
			"contents":      "write",
			"pull_requests": "write",
			"metadata":      "read",
		},
	}
}

// JSON is the form field value posted to GitHub.
func (m Manifest) JSON() (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
