package store

// DeleteWorker unregisters a stopped worker without deleting its activity history.
// Repeated calls are harmless; a subsequent piso up registers it again.
func (s *Store) DeleteWorker(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, rec := range s.state.Workers {
		if rec.Name == name {
			old := s.state.Workers
			s.state.Workers = append(append([]WorkerRec{}, old[:i]...), old[i+1:]...)
			if err := s.save(); err != nil {
				s.state.Workers = old
				return err
			}
			return nil
		}
	}
	return nil
}

// SetProjectHidden applies a dashboard preference to all workers in a project.
func (s *Store) SetProjectHidden(slug string, hidden bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := append([]WorkerRec(nil), s.state.Workers...)
	found := false
	for i := range s.state.Workers {
		if s.state.Workers[i].Slug == slug {
			s.state.Workers[i].ProjectHidden = hidden
			found = true
		}
	}
	if !found {
		return ErrWorkerNotFound
	}
	if err := s.save(); err != nil {
		s.state.Workers = old
		return err
	}
	return nil
}
