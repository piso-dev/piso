package server

import (
	"net/http"

	"piso/gateway/internal/technocore"
)

// handleTelegramAssignments reads account-wide metadata from the coordinator.
// It neither requires a locally connected worker nor starts a Telegram poller.
// Keep this separate from the local mailbox cache: a remote assignment must
// never authorize delivery through this gateway.
func (s *Server) handleTelegramAssignments(w http.ResponseWriter, r *http.Request) {
	serviceHeaders(w)
	assignments, err := technocore.TelegramAssignments(s.dataDir())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Telegram assignments could not be read from the coordinator"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bots": assignments})
}
