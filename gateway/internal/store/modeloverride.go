package store

import (
	"fmt"
	"strings"
	"time"
)

// ModelOverrideRec pins one worker to a provider from the shared pi provider
// stack. It is a per-worker selection, never a second profile: models.json
// keeps every provider for every worker, and this row only decides which one
// that worker starts on.
//
// There is no "empty provider" row: following the account default is the
// absence of an override, so deleting the row is the only way to express it
// and there is exactly one source of truth for it.
type ModelOverrideRec struct {
	Slug       string    `json:"slug"`
	ProviderID string    `json:"providerId"`
	UpdatedAt  time.Time `json:"updatedAt,omitempty"`
}

// ModelOverrides returns every override in insertion order. The slice is a
// copy, so callers may keep or mutate it.
func (s *Store) ModelOverrides() []ModelOverrideRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.state.ModelOverrides) == 0 {
		return nil
	}
	return append([]ModelOverrideRec(nil), s.state.ModelOverrides...)
}

// ModelOverrideBySlug returns one worker's override.
func (s *Store) ModelOverrideBySlug(slug string) (ModelOverrideRec, bool) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return ModelOverrideRec{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rec := range s.state.ModelOverrides {
		if rec.Slug == slug {
			return rec, true
		}
	}
	return ModelOverrideRec{}, false
}

// ModelOverrideProviderID returns the provider pinned to slug, or "" when the
// worker follows the account default.
func (s *Store) ModelOverrideProviderID(slug string) string {
	rec, ok := s.ModelOverrideBySlug(slug)
	if !ok {
		return ""
	}
	return rec.ProviderID
}

// SetModelOverride pins slug to providerID, replacing any existing override
// for that worker. A blank slug or provider is an error: use
// DeleteModelOverride to return a worker to the account default.
func (s *Store) SetModelOverride(slug, providerID string) (ModelOverrideRec, error) {
	slug = strings.TrimSpace(slug)
	providerID = strings.TrimSpace(providerID)
	if slug == "" {
		return ModelOverrideRec{}, ErrWorkerNotFound
	}
	if providerID == "" {
		return ModelOverrideRec{}, fmt.Errorf("provider required for a model override")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.state.ModelOverrides
	next := make([]ModelOverrideRec, 0, len(old)+1)
	rec := ModelOverrideRec{Slug: slug, ProviderID: providerID, UpdatedAt: time.Now().UTC()}
	replaced := false
	for _, e := range old {
		if e.Slug == slug {
			if !replaced {
				next = append(next, rec)
				replaced = true
			}
			continue
		}
		next = append(next, e)
	}
	if !replaced {
		next = append(next, rec)
	}
	s.state.ModelOverrides = next
	if err := s.save(); err != nil {
		s.state.ModelOverrides = old
		return ModelOverrideRec{}, err
	}
	return rec, nil
}

// DeleteModelOverride returns a worker to the account default. Deleting an
// override that is not there is not an error: the caller's intent (this worker
// follows the account default) already holds.
func (s *Store) DeleteModelOverride(slug string) error {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return ErrWorkerNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.state.ModelOverrides
	next := make([]ModelOverrideRec, 0, len(old))
	for _, e := range old {
		if e.Slug != slug {
			next = append(next, e)
		}
	}
	if len(next) == len(old) {
		return nil
	}
	s.state.ModelOverrides = next
	if err := s.save(); err != nil {
		s.state.ModelOverrides = old
		return err
	}
	return nil
}

// pruneModelOverrideLocked drops slug's override. The caller already holds the
// write lock and owns the save.
func (s *Store) pruneModelOverrideLocked(slug string) {
	slug = strings.TrimSpace(slug)
	if slug == "" || len(s.state.ModelOverrides) == 0 {
		return
	}
	kept := make([]ModelOverrideRec, 0, len(s.state.ModelOverrides))
	for _, rec := range s.state.ModelOverrides {
		if rec.Slug != slug {
			kept = append(kept, rec)
		}
	}
	s.state.ModelOverrides = kept
}
