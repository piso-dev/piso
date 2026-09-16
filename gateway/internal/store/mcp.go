package store

import (
	"fmt"
	"strings"
	"time"

	"piso/gateway/internal/model"
)

// MCP OAuth status values — keep in sync with model.McpStatus*.
const (
	McpStatusNeedsAuth = model.McpStatusNeedsAuth
	McpStatusOK        = model.McpStatusOK
	McpStatusExpired   = model.McpStatusExpired
	McpStatusRevoked   = model.McpStatusRevoked
)

// McpServerRec is a gateway-brokered MCP server. Real OAuth client secrets
// and tokens live here / on the linked SecretRec (gateway state.json only).
type McpServerRec struct {
	ID           string     `json:"id"`
	Worker       string     `json:"worker"`
	Slug         string     `json:"slug"`
	Name         string     `json:"name"`
	URL          string     `json:"url"`
	Host         string     `json:"host,omitempty"`
	Placeholder  string     `json:"placeholder"`
	SecretID     string     `json:"secretId"`
	Status       string     `json:"status"`
	Resource     string     `json:"resource,omitempty"`
	TokenURL     string     `json:"tokenUrl,omitempty"`
	AuthURL      string     `json:"authUrl,omitempty"`
	RegisterURL  string     `json:"registerUrl,omitempty"`
	ClientID     string     `json:"clientId,omitempty"`
	ClientSecret string     `json:"clientSecret,omitempty"`
	RefreshToken string     `json:"refreshToken,omitempty"`
	CodeVerifier string     `json:"codeVerifier,omitempty"`
	OAuthState   string     `json:"oauthState,omitempty"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// ToSummary strips OAuth secrets.
func (r McpServerRec) ToSummary() model.McpServer {
	return model.McpServer{
		ID: r.ID, Worker: r.Worker, Slug: r.Slug, Name: r.Name,
		URL: r.URL, Placeholder: r.Placeholder, Status: r.Status,
		Host: r.Host, ExpiresAt: r.ExpiresAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (s *Store) McpServers() []McpServerRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyMcpRecs(s.state.McpServers)
}

func (s *Store) McpByID(id string) (McpServerRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.McpServers {
		if r.ID == id {
			return r, true
		}
	}
	return McpServerRec{}, false
}

func (s *Store) McpByState(state string) (McpServerRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.McpServers {
		if r.OAuthState != "" && r.OAuthState == state {
			return r, true
		}
	}
	return McpServerRec{}, false
}

func (s *Store) FindMcp(worker, name, rawURL string) (McpServerRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	canon := canonicalMCPURL(rawURL)
	for _, r := range s.state.McpServers {
		if r.Worker != worker {
			continue
		}
		if (name != "" && r.Name == name) || (canon != "" && canonicalMCPURL(r.URL) == canon) {
			return r, true
		}
	}
	return McpServerRec{}, false
}

func (s *Store) McpByPlaceholder(placeholder string) (McpServerRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.McpServers {
		if r.Placeholder == placeholder {
			return r, true
		}
	}
	return McpServerRec{}, false
}

func (s *Store) PendingMcpAuth() []McpServerRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []McpServerRec
	for _, r := range s.state.McpServers {
		if r.Status == McpStatusNeedsAuth || r.Status == McpStatusExpired || r.Status == McpStatusRevoked {
			out = append(out, r)
		}
	}
	return out
}

// UpsertMcpServer inserts or returns the existing row for (worker, name) or
// (worker, url). created is true only on insert. Caller supplies a fully
// populated rec including SecretID/Placeholder for inserts.
func (s *Store) UpsertMcpServer(rec McpServerRec) (McpServerRec, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for i, existing := range s.state.McpServers {
		if existing.Worker == rec.Worker && (existing.Name == rec.Name || canonicalMCPURL(existing.URL) == canonicalMCPURL(rec.URL)) {
			existing.UpdatedAt = now
			s.state.McpServers[i] = existing
			if err := s.save(); err != nil {
				return McpServerRec{}, false, err
			}
			s.broadcastMcp(existing)
			return existing, false, nil
		}
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	s.state.McpServers = append(s.state.McpServers, rec)
	if err := s.save(); err != nil {
		return McpServerRec{}, false, err
	}
	s.broadcastMcp(rec)
	return rec, true, nil
}

func (s *Store) ReplaceMcpServer(rec McpServerRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.UpdatedAt = time.Now().UTC()
	for i, existing := range s.state.McpServers {
		if existing.ID != rec.ID {
			continue
		}
		s.state.McpServers[i] = rec
		if err := s.save(); err != nil {
			return err
		}
		s.broadcastMcp(rec)
		return nil
	}
	return fmt.Errorf("mcp server %s not found", rec.ID)
}

func (s *Store) PlaceholderTaken(placeholder string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sec := range s.state.Secrets {
		if sec.Placeholder == placeholder {
			return true
		}
	}
	for _, m := range s.state.McpServers {
		if m.Placeholder == placeholder {
			return true
		}
	}
	return false
}

func (s *Store) broadcastMcp(rec McpServerRec) {
	select {
	case s.onMcp <- rec:
	default:
	}
}

func (s *Store) SubMcp() <-chan McpServerRec { return s.onMcp }

func copyMcpRecs(in []McpServerRec) []McpServerRec {
	out := make([]McpServerRec, len(in))
	copy(out, in)
	return out
}

func canonicalMCPURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}
