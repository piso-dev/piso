package server

import (
	"context"

	"piso/gateway/internal/store"
)

// storeSecret inserts a secret and seeds the substitution rules implied by its
// AllowedHosts. Gateway-only secrets seed no rules, because
// store.SyncRulesForSecret drops the host list for them.
//
// Use saveSecret instead when the caller also wants traffic that was blocked on
// this placeholder replayed right away. Use storeSecret directly only when the
// replay has to wait for something else to be persisted first (the GitHub App
// routes need the app record to exist before captured calls can be routed).
func (s *Server) storeSecret(rec store.SecretRec) error {
	if err := s.Store.AddSecret(rec); err != nil {
		return err
	}
	return s.Store.SyncRulesForSecret(rec)
}

// seedSecretRules re-seeds rec's substitution rules and replays captures that
// were blocked on its placeholder. It is the edit path: the secret already
// exists, only its scope or hosts changed.
func (s *Server) seedSecretRules(ctx context.Context, rec store.SecretRec) ([]RetryResult, error) {
	if err := s.Store.SyncRulesForSecret(rec); err != nil {
		return nil, err
	}
	seenRetry := map[string]bool{}
	retried := []RetryResult{}
	for _, h := range store.CanonicalRuleHosts(rec.AllowedHosts) {
		retried = append(retried, s.replayMatches(ctx, seenRetry, rec.Placeholder, h)...)
	}
	return retried, nil
}

// saveSecret is the single entry point for a new secret: insert it, seed its
// rules, then unblock captured traffic. Per-service handlers that hold a
// credential for an external service must go through this rather than
// re-implementing the pipeline.
func (s *Server) saveSecret(ctx context.Context, rec store.SecretRec) ([]RetryResult, error) {
	if err := s.storeSecret(rec); err != nil {
		return nil, err
	}
	return s.seedSecretRules(ctx, rec)
}
