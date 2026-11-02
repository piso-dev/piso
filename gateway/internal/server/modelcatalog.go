package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"piso/gateway/internal/store"
)

type workerModelPolicy struct {
	Mode      string   `json:"mode"` // restricted | worker-discovered | unavailable
	Models    []string `json:"models,omitempty"`
	BaseURL   string   `json:"baseUrl"`
	Discovery string   `json:"discovery,omitempty"`
}

func localApprovedModels(p *store.PiProviderRec) []string {
	ids := []string{}
	for _, id := range append(append([]string{}, p.ApprovedModels...), p.DefaultModel) {
		id = strings.TrimSpace(id)
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// workerModelPolicies is the per-provider answer the worker extension enforces
// itself, and for a local provider it is the only enforcement there is: modelPin
// never sees a loopback request, so the gateway cannot refuse one. That is why
// the pin applies here to every provider, local included — a worker pinned to a
// remote provider that kept its local list could generate through Tinfoil while
// the dashboard said it was running somewhere else. Only the provider this
// worker starts on may come back usable; the rest are a known denial.
func (s *Server) workerModelPolicies(pinned, slug string) map[string]workerModelPolicy {
	out := map[string]workerModelPolicy{}
	for _, p := range s.Store.PiProviderStack() {
		policy := workerModelPolicy{Mode: "restricted", Models: p.AllowedModelIDs(), BaseURL: p.BaseURL}
		if piProviderIsLocal(&p) {
			policy.Models = localApprovedModels(&p)
			policy.Discovery = "worker"
			if len(policy.Models) == 0 {
				policy.Mode = "worker-discovered"
			}
			if p.ID != pinned {
				policy.Mode = "unavailable"
				policy.Models = nil
				policy.Discovery = ""
			}
		} else if p.ID != pinned || len(policy.Models) == 0 {
			policy.Mode = "unavailable"
		}
		if ok, _ := s.providerReachableFrom(&p, slug); !ok {
			policy.Mode = "unavailable"
			policy.Models = nil
		}
		out[p.ID] = policy
	}
	return out
}

func (s *Server) handleWorkerPostModelCatalog(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Worker   string   `json:"worker"`
		Provider string   `json:"provider"`
		BaseURL  string   `json:"baseUrl"`
		Models   []string `json:"models"`
		Failed   bool     `json:"failed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid catalog report"})
		return
	}
	// Unlike advisory context labels, catalogs require a registered identity.
	worker, ok := s.Store.WorkerByIP(requestIP(r))
	if !ok || worker.Name != in.Worker {
		writeJSON(w, 403, map[string]string{"error": "worker identity required"})
		return
	}
	p := s.Store.PiProviderByID(in.Provider)
	if !piProviderIsLocal(p) || p.BaseURL != in.BaseURL {
		writeJSON(w, 400, map[string]string{"error": "local provider configuration mismatch"})
		return
	}
	if ok, why := s.providerReachableFrom(p, worker.Slug); !ok {
		writeJSON(w, 403, map[string]string{"error": why})
		return
	}
	if len(in.Models) > 1000 {
		writeJSON(w, 400, map[string]string{"error": "too many models"})
		return
	}
	ids := []string{}
	for _, id := range in.Models {
		id = strings.TrimSpace(id)
		if id == "" || len(id) > 256 || strings.IndexFunc(id, unicode.IsControl) >= 0 {
			writeJSON(w, 400, map[string]string{"error": "invalid model id"})
			return
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	rec := store.WorkerModelCatalog{Worker: worker.Name, Provider: p.ID, BaseURL: p.BaseURL, SecretID: p.SecretID, Models: ids}
	if in.Failed {
		rec.Error = "worker model discovery failed"
	}
	if err := s.Store.SetWorkerModelCatalog(rec); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not store catalog"})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// Reports are per-worker and advisory. Never feed them into p.Models.
func (s *Server) localCatalog(p *store.PiProviderRec) piProviderCatalog {
	out := piProviderCatalog{Catalog: "local", Models: []string{}}
	if !piProviderIsLocal(p) {
		return out
	}
	for _, rec := range s.Store.WorkerModelCatalogs(p.ID) {
		worker, ok := s.Store.WorkerByName(rec.Worker)
		if !ok || rec.BaseURL != p.BaseURL || rec.SecretID != p.SecretID {
			continue
		}
		if ok, _ := s.providerReachableFrom(p, worker.Slug); !ok {
			continue
		}
		out.Reports = append(out.Reports, rec)
		for _, id := range rec.Models {
			if !slices.Contains(out.Models, id) {
				out.Models = append(out.Models, id)
			}
		}
		if rec.CheckedAt.Format(time.RFC3339) > out.CheckedAt {
			out.CheckedAt = rec.CheckedAt.Format(time.RFC3339)
		}
	}
	slices.Sort(out.Models)
	return out
}

func (s *Server) localCatalogModels(p *store.PiProviderRec) []string { return s.localCatalog(p).Models }
