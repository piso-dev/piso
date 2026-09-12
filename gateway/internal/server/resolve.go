package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
)

// ResolveEntry is one aggregator row: env name + real value for a host.
type ResolveEntry struct {
	Name        string `json:"name"`
	EnvKey      string `json:"envKey"`
	Value       string `json:"value"`
	Placeholder string `json:"placeholder"`
	Host        string `json:"host"`
}

// ResolveRequest is POST /api/v1/failures/resolve.
type ResolveRequest struct {
	Entries []ResolveEntry `json:"entries"`
}

// ResolveResponse is the batch outcome plus worker env keys written.
type ResolveResponse struct {
	Results []RetryResult `json:"results"`
	EnvKeys []string      `json:"envKeys"`
}

func (s *Server) handleResolveFailures(w http.ResponseWriter, r *http.Request) {
	var in ResolveRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	results, envKeys, err := s.resolveFailures(r.Context(), in.Entries)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, ResolveResponse{Results: results, EnvKeys: envKeys})
}

func (s *Server) resolveFailures(ctx context.Context, entries []ResolveEntry) ([]RetryResult, []string, error) {
	var results []RetryResult
	seenRetry := map[string]bool{}
	for _, e := range entries {
		e.Name = strings.TrimSpace(e.Name)
		e.EnvKey = strings.TrimSpace(e.EnvKey)
		e.Placeholder = strings.TrimSpace(e.Placeholder)
		e.Host = strings.TrimSpace(e.Host)
		e.Value = strings.TrimSpace(e.Value)
		if e.Value == "" {
			continue
		}
		if e.EnvKey == "" || !store.ValidEnvKey(e.EnvKey) {
			return nil, nil, fmt.Errorf("invalid envKey %q", e.EnvKey)
		}
		if e.Host == "" {
			e.Host = "*"
		}
		if e.Name == "" {
			e.Name = e.EnvKey
		}
		if e.Placeholder == "" {
			e.Placeholder = model.PlaceholderPrefix + slugToken(e.Name) + "_" + randID()
		}
		if !strings.HasPrefix(e.Placeholder, model.PlaceholderPrefix) {
			return nil, nil, fmt.Errorf("placeholder must start with %s", model.PlaceholderPrefix)
		}

		sec, ok := s.Store.SecretByPlaceholder(e.Placeholder)
		if !ok {
			sec = store.SecretRec{
				ID: "sec_" + randID(), Name: e.Name, Placeholder: e.Placeholder,
				Value: e.Value, EnvKey: e.EnvKey,
				AllowedHosts: hostsFor(e.Host), CreatedAt: time.Now().UTC(),
			}
			if err := s.Store.AddSecret(sec); err != nil {
				return nil, nil, err
			}
		} else if e.EnvKey != "" && sec.EnvKey != e.EnvKey {
			if err := s.Store.SetEnvKey(sec.ID, e.EnvKey); err != nil {
				return nil, nil, err
			}
			sec.EnvKey = e.EnvKey
		}

		if err := s.Store.AddRule(store.RuleRec{
			ID: "rule_" + randID(), SecretID: sec.ID,
			Host: e.Host, Placeholder: e.Placeholder,
		}); err != nil {
			return nil, nil, err
		}

		results = append(results, s.replayMatches(ctx, seenRetry, e.Placeholder, e.Host)...)
	}

	envKeys := []string{}
	for _, rec := range s.Store.Secrets() {
		if rec.EnvKey != "" {
			envKeys = append(envKeys, rec.EnvKey)
		}
	}
	return results, envKeys, nil
}

func hostsFor(host string) []string {
	if host == "" || host == "*" {
		return nil
	}
	return []string{host}
}

func slugToken(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else if r == '_' || r == '-' {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "secret"
	}
	if len(out) > 24 {
		return out[:24]
	}
	return out
}
