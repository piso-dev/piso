package technocore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

var localPrincipalID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// EnrollLocalWorker is called by host registration. Failures leave the local
// worker usable; the background reconciler retries enrollment independently.
func EnrollLocalWorker(st *store.Store, dataDir string, worker store.WorkerRec) error {
	if worker.Remote || worker.PrincipalID != "" {
		return nil
	}
	id, err := tc.Load(tc.Path(dataDir))
	if err != nil {
		return ErrUnpaired
	}
	return enrollLocalWorker(st, id, worker)
}

func enrollLocalWorker(st *store.Store, id tc.Identity, worker store.WorkerRec) error {
	body, _ := json.Marshal(map[string]string{"slug": worker.Slug})
	status, payload, err := Call(id, http.MethodPost, "/api/v1/local-workers", nil, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("local worker enrollment HTTP %d", status)
	}
	var out struct {
		PrincipalID string `json:"principal_id"`
		Slug        string `json:"slug"`
	}
	if json.Unmarshal(payload, &out) != nil || !localPrincipalID.MatchString(out.PrincipalID) || out.PrincipalID == id.PrincipalID || out.Slug != worker.Slug {
		return fmt.Errorf("invalid local worker enrollment response")
	}
	return st.BindLocalWorkerPrincipal(worker.Name, worker.Slug, out.PrincipalID)
}

func syncLocalWorkers(st *store.Store, id tc.Identity) error {
	var first error
	for _, worker := range st.Workers() {
		if worker.Remote || worker.PrincipalID != "" {
			continue
		}
		if err := enrollLocalWorker(st, id, worker); err != nil && first == nil {
			first = err
		}
	}
	return first
}
