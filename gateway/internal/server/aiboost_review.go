package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"piso/gateway/internal/aiboost"
)

func boostReviewError(w http.ResponseWriter, err error) {
	status, message := 502, "AI Boost review unavailable. Refresh before retrying."
	var upstream *aiboost.APIError
	switch {
	case errors.Is(err, aiboost.ErrReviewInput):
		status, message = 400, err.Error()
	case errors.Is(err, aiboost.ErrReconnect):
		status, message = 401, "Reconnect AI Boost in the dashboard."
	case errors.As(err, &upstream):
		switch upstream.Status {
		case 400:
			status, message = 400, "AI Boost rejected the review parameters."
		case 401, 403:
			status, message = 403, "AI Boost denied review access. Reconnect AI Boost to authorize dashboard review."
		case 404:
			status, message = 404, "Suggestion not found in this connection."
		case 409:
			status, message = 409, "Suggestion changed or was already decided. Close and reopen the preview before deciding again."
		case 429:
			status, message = 429, "AI Boost rate limit reached. Try again later."
		}
	}
	http.Error(w, message, status)
}
func (s *Server) handleBoostSuggestions(w http.ResponseWriter, r *http.Request) {
	boostHeaders(w)
	c, err := s.boostClient()
	if err != nil {
		boostReviewError(w, err)
		return
	}
	page, err := c.Suggestions(r.Context(), r.URL.Query().Get("status"), r.URL.Query().Get("cursor"))
	if err != nil {
		boostReviewError(w, err)
		return
	}
	writeJSON(w, 200, page)
}
func (s *Server) handleBoostSuggestion(w http.ResponseWriter, r *http.Request) {
	boostHeaders(w)
	c, err := s.boostClient()
	if err != nil {
		boostReviewError(w, err)
		return
	}
	item, err := c.GetSuggestion(r.Context(), r.PathValue("id"))
	if err != nil {
		boostReviewError(w, err)
		return
	}
	writeJSON(w, 200, item)
}
func (s *Server) handleBoostDecision(w http.ResponseWriter, r *http.Request) {
	boostHeaders(w)
	var in aiboost.Decision
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || dec.Decode(&struct{}{}) != io.EOF {
		boostReviewError(w, aiboost.ErrReviewInput)
		return
	}
	c, err := s.boostClient()
	if err != nil {
		boostReviewError(w, err)
		return
	}
	item, err := c.Decide(r.Context(), r.PathValue("id"), r.PathValue("action"), in)
	if err != nil {
		boostReviewError(w, err)
		return
	}
	writeJSON(w, 200, item)
}
