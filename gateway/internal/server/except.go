package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"piso/gateway/internal/store"
)

// ExceptRequest is POST /api/v1/failures/except: allow one pattern on one host.
type ExceptRequest struct {
	Host      string `json:"host"`
	PatternID string `json:"patternId"`
	Note      string `json:"note,omitempty"`
}

// ExceptResponse is the upserted exception plus sequential replay outcomes.
type ExceptResponse struct {
	Exception store.ExceptionRec `json:"exception"`
	Created   bool               `json:"created"`
	Results   []RetryResult      `json:"results"`
	Updated   int                `json:"updated"`
}

func (s *Server) handleExceptFailures(w http.ResponseWriter, r *http.Request) {
	var in ExceptRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	exc, created, results, updated, err := s.exceptFailures(r.Context(), in)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, ExceptResponse{Exception: exc, Created: created, Results: results, Updated: updated})
}

func (s *Server) exceptFailures(ctx context.Context, in ExceptRequest) (store.ExceptionRec, bool, []RetryResult, int, error) {
	in.Host = strings.TrimSpace(in.Host)
	in.PatternID = strings.TrimSpace(in.PatternID)
	in.Note = strings.TrimSpace(in.Note)
	if in.Host == "" {
		return store.ExceptionRec{}, false, nil, 0, fmt.Errorf("host required")
	}
	if in.PatternID == "" {
		return store.ExceptionRec{}, false, nil, 0, fmt.Errorf("patternId required")
	}

	hostRegex := hostExceptionRegex(in.Host)
	created := false
	rec, ok := existingHostPatternException(s.Store.Exceptions(), hostRegex, in.PatternID)
	if !ok {
		rec = store.ExceptionRec{
			ID:        "exc_" + randID(),
			HostRegex: hostRegex,
			PatternID: in.PatternID,
			Note:      in.Note,
			Enabled:   true,
		}
		if rec.Note == "" {
			rec.Note = "allow " + in.PatternID + " on " + in.Host
		}
		if err := s.Store.AddException(rec); err != nil {
			return store.ExceptionRec{}, false, nil, 0, err
		}
		created = true
	}
	results := s.replayPatternMatches(ctx, in.PatternID, in.Host)
	updated := s.Store.ApplyAllowedException(in.PatternID, in.Host)
	return rec, created, results, len(updated), nil
}

func (s *Server) replayPatternMatches(ctx context.Context, patternID, host string) []RetryResult {
	var results []RetryResult
	for _, rec := range matchingRetryablePattern(s.Store.Records(0), patternID, host) {
		fresh, ok := s.Store.ReplayGet(rec.ID)
		if !ok {
			results = append(results, RetryResult{ID: rec.ID, Error: "gone"})
			continue
		}
		results = append(results, s.replayOne(ctx, fresh))
	}
	return results
}

func hostExceptionRegex(host string) string {
	return "^" + regexp.QuoteMeta(host) + "$"
}

func existingHostPatternException(es []store.ExceptionRec, hostRegex, patternID string) (store.ExceptionRec, bool) {
	for _, e := range es {
		if e.Enabled && e.HostRegex == hostRegex && e.PatternID == patternID {
			return e, true
		}
	}
	return store.ExceptionRec{}, false
}
