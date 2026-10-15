package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"piso/gateway/internal/store"
	"piso/gateway/internal/technocore"
	"piso/gateway/internal/telegram"
)

// supervisorTTL is the window a worker asks the coordinator for. It matches the
// Telegram poller lease: the supervisor serves that front door, so the two
// lifetimes should not drift.
const supervisorTTL = telegram.LeaseTTL

// supervisorIn is a worker's candidacy claim. Identity is checked against the
// slug<->IP registry like every other worker route, so a worker cannot nominate
// another.
type supervisorIn struct {
	Worker string `json:"worker"`
	Slug   string `json:"slug,omitempty"`
}

// supervisorOut is the answer the worker polls for. Reason is empty on a grant.
type supervisorOut struct {
	Granted   bool       `json:"granted"`
	Reason    string     `json:"reason,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// handleWorkerSupervisor is a worker's supervisor-lease ask, relayed to the
// coordinator. The gateway is a worker's only route to the coordinator, and the
// only holder of the Telegram poller lease, so the worker asks here and the
// gateway signs the coordinator call with the worker's attested principal.
//
// The gateway does not arbitrate: it forwards, caches the winner for the
// dashboard and the delivery path, and answers with the coordinator's decision.
// A refusal is a 200 with granted=false so the worker keeps polling instead of
// treating contention as a failure.
func (s *Server) handleWorkerSupervisor(w http.ResponseWriter, r *http.Request) {
	var in supervisorIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Worker = strings.TrimSpace(in.Worker)
	in.Slug = strings.TrimSpace(in.Slug)
	if !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "worker name required"})
		return
	}
	if in.Slug == "" {
		in.Slug = slugFromWorker(in.Worker)
	}
	if reg, ok := s.Store.WorkerByIP(requestIP(r)); ok && reg.Name != in.Worker {
		identityMismatch(w, r, reg, in.Worker, in.Slug)
		return
	}
	rec, ok := s.Store.WorkerByName(in.Worker)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "worker not found"})
		return
	}
	if rec.PrincipalID == "" {
		writeJSON(w, 409, map[string]string{"error": "worker has no coordinator identity yet"})
		return
	}
	bot, ok := telegram.Secret(s.Store.Secrets())
	if !ok || bot.TechnocoreID == "" {
		writeJSON(w, 409, map[string]string{"error": "no published Telegram bot to supervise"})
		return
	}
	grant, err := technocore.SupervisorLease(s.dataDir(), bot.TechnocoreID, rec.PrincipalID, supervisorTTL)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "coordinator unreachable"})
		return
	}
	if grant.Granted {
		_ = s.Store.SetSupervisorLink(store.SupervisorLink{
			Worker:      rec.Name,
			PrincipalID: rec.PrincipalID,
			Slug:        rec.Slug,
			ExpiresAt:   grant.ExpiresAt,
		})
	} else if current := s.Store.SupervisorLink(); current.PrincipalID == rec.PrincipalID {
		// This worker was the supervisor; the coordinator has taken it away.
		// Clearing ExpiresAt is what makes the cache stop reporting it live.
		_ = s.Store.SetSupervisorLink(store.SupervisorLink{
			Worker: rec.Name, PrincipalID: rec.PrincipalID, Slug: rec.Slug,
			Reason: grant.Reason,
		})
	}
	out := supervisorOut{Granted: grant.Granted, Reason: grant.Reason}
	if grant.Granted && !grant.ExpiresAt.IsZero() {
		expires := grant.ExpiresAt
		out.ExpiresAt = &expires
	}
	writeJSON(w, 200, out)
}
