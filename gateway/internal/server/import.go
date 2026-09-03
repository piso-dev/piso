package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
)

// PiKeyProvider is one host models.json provider (real apiKey, never logged).
type PiKeyProvider struct {
	Name   string `json:"name"`
	APIKey string `json:"apiKey"`
	Host   string `json:"host"`
}

// PiKeysRequest is POST /api/v1/imports/pi-keys.
type PiKeysRequest struct {
	Providers []PiKeyProvider `json:"providers"`
}

// PiKeyResult is the log-safe outcome for one provider (no apiKey).
type PiKeyResult struct {
	Name        string `json:"name"`
	Placeholder string `json:"placeholder"`
	EnvKey      string `json:"envKey"`
	Host        string `json:"host"`
}

// PiKeysResponse lists imported providers without values.
type PiKeysResponse struct {
	Providers []PiKeyResult `json:"providers"`
}

func (s *Server) handleImportPiKeys(w http.ResponseWriter, r *http.Request) {
	var in PiKeysRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	out, err := s.importPiKeys(in.Providers)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, PiKeysResponse{Providers: out})
}

func (s *Server) importPiKeys(providers []PiKeyProvider) ([]PiKeyResult, error) {
	var out []PiKeyResult
	for _, p := range providers {
		p.Name = strings.TrimSpace(p.Name)
		p.APIKey = strings.TrimSpace(p.APIKey)
		p.Host = strings.TrimSpace(p.Host)
		if p.Name == "" || p.APIKey == "" {
			continue
		}
		if strings.HasPrefix(p.APIKey, model.PlaceholderPrefix) {
			continue
		}
		host := importHost(p.Host)
		envKey := providerEnvKey(p.Name)
		if !store.ValidEnvKey(envKey) {
			continue
		}
		rec, err := s.upsertPiSecret(p.Name, envKey, p.APIKey, host)
		if err != nil {
			return nil, err
		}
		if host != "" && !s.Store.HasRule(rec.Placeholder, host) {
			if err := s.Store.AddRule(store.RuleRec{
				ID: "rule_" + randID(), SecretID: rec.ID,
				Host: host, Placeholder: rec.Placeholder,
			}); err != nil {
				return nil, err
			}
		}
		out = append(out, PiKeyResult{
			Name: p.Name, Placeholder: rec.Placeholder, EnvKey: rec.EnvKey, Host: host,
		})
	}
	return out, nil
}

func (s *Server) upsertPiSecret(name, envKey, value, host string) (store.SecretRec, error) {
	if existing, ok := s.Store.SecretByEnvKey(envKey); ok {
		existing.Value = value
		existing.Name = name
		if host != "" {
			existing.AllowedHosts = mergeHost(existing.AllowedHosts, host)
		}
		if err := s.Store.ReplaceSecret(existing); err != nil {
			return store.SecretRec{}, err
		}
		return existing, nil
	}
	hosts := []string(nil)
	if host != "" {
		hosts = []string{host}
	}
	rec := store.SecretRec{
		ID: "sec_" + randID(), Name: name,
		Placeholder: model.PlaceholderPrefix + slugToken(name) + "_" + randID(),
		Value:       value, EnvKey: envKey, AllowedHosts: hosts,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.Store.AddSecret(rec); err != nil {
		return store.SecretRec{}, err
	}
	return rec, nil
}

func importHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		return strings.ToLower(strings.Split(raw, "/")[0])
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func providerEnvKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else if r == '_' || r == '-' {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "PI_API_KEY"
	}
	return out + "_API_KEY"
}

func mergeHost(hosts []string, host string) []string {
	for _, h := range hosts {
		if strings.EqualFold(h, host) {
			return hosts
		}
	}
	return append(append([]string{}, hosts...), host)
}
