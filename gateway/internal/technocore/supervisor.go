package technocore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	tc "piso/internal/technocore"
)

// SupervisorGrant is the coordinator's answer to a supervisor-lease request.
// A refusal is an answer, not a fault: Reason names which gate refused, so the
// caller can wait for a different one instead of backing off as if it failed.
type SupervisorGrant struct {
	// Granted is false when the caller is not the supervisor, now or yet.
	Granted bool
	// Reason is the refusal: gateway_no_telegram_lease, not_standalone, or
	// supervisor_taken. Empty on a grant.
	Reason string
	// Holder and HolderPrincipal identify the worker that holds the lease when
	// the caller was refused.
	Holder          string
	HolderPrincipal string
	// ExpiresAt bounds how long the holder may supervise without renewing.
	ExpiresAt time.Time
}

// SupervisorLease asks the coordinator to name one of this gateway's workers as
// the supervisor of a Telegram bot. workerID is the worker's coordinator
// principal id, which the gateway attests: the coordinator uses it to check the
// worker belongs to the calling gateway and is standalone.
//
// One call both takes a free lease and renews a held one. A refusal is returned
// as a grant with Granted=false, never as an error.
func SupervisorLease(dataDir, resourceID, workerID string, ttl time.Duration) (SupervisorGrant, error) {
	id, err := tc.Load(tc.Path(dataDir))
	if err != nil {
		return SupervisorGrant{}, err
	}
	body, err := json.Marshal(map[string]any{
		"worker_id": workerID,
		"ttl":       int(ttl.Seconds()),
	})
	if err != nil {
		return SupervisorGrant{}, err
	}
	path := "/api/v1/telegram/bots/" + url.PathEscape(resourceID) + "/supervisor-lease"
	code, raw, err := Call(id, http.MethodPost, path, nil, body)
	if err != nil {
		return SupervisorGrant{}, err
	}
	if code != http.StatusOK {
		return SupervisorGrant{}, fmt.Errorf("supervisor lease: HTTP %d: %s", code, raw)
	}
	var out struct {
		Granted         bool   `json:"granted"`
		Reason          string `json:"reason"`
		Holder          string `json:"holder"`
		HolderPrincipal string `json:"holder_principal"`
		ExpiresAt       string `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return SupervisorGrant{}, fmt.Errorf("supervisor lease: %w", err)
	}
	grant := SupervisorGrant{
		Granted:         out.Granted,
		Reason:          out.Reason,
		Holder:          out.Holder,
		HolderPrincipal: out.HolderPrincipal,
	}
	// A missing or unparseable expiry is not fatal: the caller falls back to
	// its own requested window.
	if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
		grant.ExpiresAt = t
	}
	return grant, nil
}
