package store

import "fmt"

// BindLocalWorkerPrincipal updates only the coordinator mapping, never transport
// identity or IPs. Refuse a stale response after deletion, rename or remapping.
func (s *Store) BindLocalWorkerPrincipal(name, slug, principal string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if principal == "" {
		return fmt.Errorf("empty worker principal")
	}
	for i := range s.state.Workers {
		r := &s.state.Workers[i]
		if r.Name != name || r.Slug != slug || r.Remote {
			continue
		}
		if r.PrincipalID != "" && r.PrincipalID != principal {
			return fmt.Errorf("local worker principal conflict")
		}
		r.PrincipalID = principal
		return s.save()
	}
	return ErrWorkerNotFound
}
