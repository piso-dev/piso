package store

import (
	"fmt"
	"time"
)

// ErrIngressNotFound is a missing pending ingress request.
var ErrIngressNotFound = fmt.Errorf("ingress request not found")

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
	return rec, true, nil
}

// ApproveIngress publishes a live route for a pending request and drops the
// inbox row. Same-worker name reuse updates the route; another worker's name
// is ErrIngressLabelTaken.
func (s *Store) ApproveIngress(id, routeID string) (IngressRequestRec, RouteRec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	out := RouteRec{ID: routeID, Name: rec.Name, Worker: rec.Worker, Port: rec.Port, Note: rec.Note}
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

// CancelPendingIngress drops pending rows for worker + kind (port went down).
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
	if n == 0 {
		return 0
	}
	s.state.IngressRequests = out
	_ = s.save()
	return n
}
