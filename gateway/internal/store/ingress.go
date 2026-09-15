package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Auto-route policy for worker-published ingress (the ports watcher).
// Auto routes are named <slug>-<port> (AutoRouteName): slugs are unique per
// host and ports within a worker, so labels cannot collide across workers
// (the planning namespace plan-<slug> stays separate). A route's Origin+LastSeen
// drive the dashboard live/down badge and the stale sweep in Routes().
const (
	AutoRouteOriginExpose = "expose" // host-created (`piso expose` / UI) — sticky, never swept
	AutoRouteOriginAuto   = "auto"   // worker-published — rate-capped, swept
	AutoRouteOriginPlan   = "plan"   // approved planning route — GC'd like auto (no heartbeat source)
	// Down threshold for the live badge: no watcher heartbeat for 45s.
	AutoRouteDownMs = 45000
	// Stale sweep: an auto or plan route disappears after 10 min with no
	// heartbeat for its port (dev servers restart constantly — never delete
	// on a blip). Expose routes are sticky and never swept.
	AutoRouteGraceMs = 600000
	MaxAutoRoutesPerWorker = 20
)

// AutoRouteName is the deterministic label for a worker-published route:
// <slug>-<port>. DNS-safe and capped at 63 chars.
func AutoRouteName(slug string, port int) string {
	label := dnsLabel(slug)
	if label == "" {
		label = "worker"
	}
	name := label + "-" + fmt.Sprintf("%d", port)
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

// dnsLabel lowercases and collapses to [a-z0-9-] (one dash per run, trimmed).
func dnsLabel(raw string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(raw) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if b.Len() > 0 && !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// SyncWorkerPorts reconciles a worker's auto routes against the reachable
// port set its ports watcher just reported. The watcher posts the FULL set
// (what is listening — never what stopped), so this is a create/refresh loop
// with no deletes: a port that left the set stops refreshing LastSeen and the
// stale sweep in Routes() drops its route after the down-grace. An existing
// route with the same label is refreshed when it targets the same worker and
// never replaced when it is an expose route (expose wins). Returns the routes
// created on this call (broadcast for SSE).
func (s *Store) SyncWorkerPorts(worker, slug string, ports []int) ([]RouteRec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixNano()/1000000
	// Sort so the per-worker rate cap applies to the lowest ports first.
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	alive := map[int]RouteRec{}
	created := make([]RouteRec, 0, len(ports))
	for _, p := range ports {
		if len(alive) >= MaxAutoRoutesPerWorker {
			break
		}
		name := AutoRouteName(slug, p)
		found := false
		for i, r := range s.state.Routes {
			if r.Name != name {
				continue
			}
			found = true
			if r.Worker == worker {
				// The heartbeat proves the port is reachable, so refresh the
				// badge timestamp for expose routes too — they just never get
				// replaced or GC'd (expose wins).
				s.state.Routes[i].LastSeenMs = now
				alive[p] = s.state.Routes[i]
			}
			break
		}
		if !found {
			r := RouteRec{
				ID: "route_" + randID(), Name: name, Worker: worker, Port: p,
				Origin: AutoRouteOriginAuto, LastSeenMs: now,
			}
			s.state.Routes = append(s.state.Routes, r)
			alive[p] = r
			created = append(created, r)
		}
	}
	if err := s.save(); err != nil {
		return nil, err
	}
	for _, r := range created {
		s.broadcastRoute(r)
	}
	return created, nil
}

func randID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// ErrIngressNotFound is a missing pending ingress request.
var ErrIngressNotFound = fmt.Errorf("ingress request not found")

// ErrRouteNotFound is a missing ingress route (by id).
var ErrRouteNotFound = fmt.Errorf("route not found")

// ErrWorkerNotFound is a missing worker-registry entry.
var ErrWorkerNotFound = fmt.Errorf("worker not found")

// ErrIngressLabelTaken is a planning label owned by another worker.
var ErrIngressLabelTaken = fmt.Errorf("ingress label in use")

// UpsertPendingIngress inserts or refreshes a pending request keyed by
// worker + kind. created is true when this call allocated a new id.
func (s *Store) UpsertPendingIngress(rec IngressRequestRec) (IngressRequestRec, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if rec.Kind == "" {
		rec.Kind = IngressKindPlanning
	}
	if rec.Status == "" {
		rec.Status = IngressStatusPending
	}
	if rec.Port == 0 {
		rec.Port = DefaultPlanningPort
	}
	for i, existing := range s.state.IngressRequests {
		if existing.Worker != rec.Worker || existing.Kind != rec.Kind || existing.Status != IngressStatusPending {
			continue
		}
		existing.Port = rec.Port
		existing.Name = rec.Name
		existing.Slug = rec.Slug
		existing.Note = rec.Note
		existing.UpdatedAt = now
		s.state.IngressRequests[i] = existing
		if err := s.save(); err != nil {
			return IngressRequestRec{}, false, err
		}
		return existing, false, nil
	}
	if rec.ID == "" {
		return IngressRequestRec{}, false, fmt.Errorf("ingress request id required")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	s.state.IngressRequests = append(s.state.IngressRequests, rec)
	if err := s.save(); err != nil {
		return IngressRequestRec{}, false, err
	}
	// Genuinely new plan: tell SSE subscribers immediately. Non-blocking
	// (drop-if-full), safe under the store lock.
	s.broadcastIngress(rec)
	return rec, true, nil
}

// ApproveIngress publishes a live route for a pending request and drops the
// inbox row. Same-worker name reuse updates the route; another worker's name
// is ErrIngressLabelTaken.
func (s *Store) ApproveIngress(id, routeID string) (IngressRequestRec, RouteRec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := timeNowMs()
	idx := -1
	var rec IngressRequestRec
	for i, r := range s.state.IngressRequests {
		if r.ID == id {
			idx = i
			rec = r
			break
		}
	}
	if idx < 0 || rec.Status != IngressStatusPending {
		return IngressRequestRec{}, RouteRec{}, ErrIngressNotFound
	}
	for _, route := range s.state.Routes {
		if route.Name == rec.Name && route.Worker != rec.Worker {
			return IngressRequestRec{}, RouteRec{}, fmt.Errorf("%w: %s belongs to %s", ErrIngressLabelTaken, rec.Name, route.Worker)
		}
	}
	if routeID == "" {
		return IngressRequestRec{}, RouteRec{}, fmt.Errorf("route id required")
	}
	// Plan routes are Origin=plan: GC'd by the stale sweep (no watcher ever
	// heartbeats the planning port, so LastSeen is set once here and the route
	// expires AutoRouteGraceMs after approval unless cancelled/deleted sooner).
	out := RouteRec{ID: routeID, Name: rec.Name, Worker: rec.Worker, Port: rec.Port, Note: rec.Note, Origin: AutoRouteOriginPlan, LastSeenMs: now}
	replaced := false
	for i, route := range s.state.Routes {
		if route.Name != rec.Name {
			continue
		}
		out.ID = route.ID
		s.state.Routes[i] = out
		replaced = true
		break
	}
	if !replaced {
		s.state.Routes = append(s.state.Routes, out)
	}
	s.state.IngressRequests = append(s.state.IngressRequests[:idx], s.state.IngressRequests[idx+1:]...)
	if err := s.save(); err != nil {
		return IngressRequestRec{}, RouteRec{}, err
	}
	return rec, out, nil
}

// DismissIngress drops a pending inbox row without creating a route.
func (s *Store) DismissIngress(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state.IngressRequests[:0]
	found := false
	for _, r := range s.state.IngressRequests {
		if r.ID == id {
			found = true
			continue
		}
		out = append(out, r)
	}
	if !found {
		return ErrIngressNotFound
	}
	s.state.IngressRequests = out
	return s.save()
}

// CancelPendingIngress is the full teardown for a worker's planning surface:
// it drops the pending inbox row(s) for worker + kind AND removes the already-
// live plan route (plan-<slug>) belonging to that worker, broadcasting each
// removal so the host sync drops the DNS entry promptly. Returns the number
// of rows + routes removed.
func (s *Store) CancelPendingIngress(worker, kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "" {
		kind = IngressKindPlanning
	}
	out := s.state.IngressRequests[:0]
	n := 0
	for _, r := range s.state.IngressRequests {
		if r.Worker == worker && r.Kind == kind && r.Status == IngressStatusPending {
			n++
			continue
		}
		out = append(out, r)
	}
	s.state.IngressRequests = out
	// Also drop the worker's live plan route(s). Match by owner + the plan-
	// prefix so we never touch a dev-server auto route with the same slug.
	if kind == IngressKindPlanning {
		rt := s.state.Routes[:0]
		for _, r := range s.state.Routes {
			if r.Worker == worker && strings.HasPrefix(r.Name, "plan-") {
				s.broadcastRoute(r)
				n++
				continue
			}
			rt = append(rt, r)
		}
		s.state.Routes = rt
	}
	if n == 0 {
		return 0
	}
	_ = s.save()
	return n
}
