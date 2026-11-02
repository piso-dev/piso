package store

import "time"

// WorkerModelCatalog is advisory discovery, never input to approval/default resolution.
// One bounded report per worker/provider; configuration changes invalidate it.
type WorkerModelCatalog struct {
	Worker       string    `json:"worker"`
	Provider     string    `json:"provider"`
	BaseURL      string    `json:"baseUrl"`
	SecretID     string    `json:"secretId"`
	Models       []string  `json:"models"`
	Error        string    `json:"error,omitempty"`
	CheckedAt    time.Time `json:"checkedAt"`
	SuccessfulAt time.Time `json:"successfulAt,omitempty"`
}

func (s *Store) SetWorkerModelCatalog(rec WorkerModelCatalog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.CheckedAt = time.Now().UTC()
	if rec.Error == "" {
		rec.SuccessfulAt = rec.CheckedAt
	}
	for i, old := range s.state.ModelCatalogs {
		if old.Worker == rec.Worker && old.Provider == rec.Provider {
			if rec.Error != "" && old.BaseURL == rec.BaseURL && old.SecretID == rec.SecretID {
				rec.Models = append([]string(nil), old.Models...)
				rec.SuccessfulAt = old.SuccessfulAt
			}
			s.state.ModelCatalogs[i] = rec
			return s.save()
		}
	}
	s.state.ModelCatalogs = append(s.state.ModelCatalogs, rec)
	return s.save()
}

func (s *Store) WorkerModelCatalogs(provider string) []WorkerModelCatalog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []WorkerModelCatalog{}
	for _, rec := range s.state.ModelCatalogs {
		if rec.Provider == provider {
			rec.Models = append([]string(nil), rec.Models...)
			out = append(out, rec)
		}
	}
	return out
}
