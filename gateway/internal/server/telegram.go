package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
	"piso/gateway/internal/telegram"
)

// telegramSecretName is the display name of the bot-token secret. It is a
// gateway-only secret: the token is never exported to a worker and never
// substituted, so there is no placeholder and no worker-facing surface.
const telegramSecretName = "Telegram bot"

// getMeTimeout bounds the token validation call, which the dashboard waits on.
const getMeTimeout = 10 * time.Second

// telegramStatus is the dashboard view of the front door. It never carries the
// token, and it never triggers an outbound call: the poller already holds the
// bot identity and the liveness signal, so reading the tab costs nothing.
type telegramStatus struct {
	Connected bool      `json:"connected"`
	Name      string    `json:"name,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
	// Running is false while the poller has no bot, is backing off, or has not
	// completed its first getMe since the gateway started.
	Running     bool       `json:"running"`
	BotID       int64      `json:"botId,omitempty"`
	BotUsername string     `json:"botUsername,omitempty"`
	LastPoll    *time.Time `json:"lastPoll,omitempty"`
	// Warning is set when the credential is present but the front door is not
	// serving. The tab stays "connected" in that case: the token is stored, only
	// the live connection is down.
	Warning        string     `json:"warning,omitempty"`
	ClaimCode      string     `json:"claimCode,omitempty"`
	ClaimExpiresAt *time.Time `json:"claimExpiresAt,omitempty"`
	// Users is the allow-list. It carries no credential, so the store record is
	// the payload.
	Users []store.TelegramUser `json:"users,omitempty"`
}

// telegramManager returns the front door. A Server built without New has none,
// so the read paths degrade to the credential-only view instead of panicking.
func (s *Server) telegramManager() *telegram.Manager {
	if s.Telegram == nil {
		return telegram.NewManager(s.Store, s.HTTP)
	}
	return s.Telegram
}

func (s *Server) registerTelegram(mux *http.ServeMux) {
	// Same treatment as the other per-service control routes: the surrounding
	// handler is host-only, and CrossOriginProtection rejects browser CSRF.
	csrf := http.NewCrossOriginProtection()
	mount := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, csrf.Handler(h)) }
	mount("GET /api/v1/telegram", s.handleTelegramStatus)
	mount("POST /api/v1/telegram", s.handleTelegramConnect)
	mount("DELETE /api/v1/telegram", s.handleTelegramDisconnect)
	mount("POST /api/v1/telegram/claim", s.handleTelegramClaim)
	mount("DELETE /api/v1/telegram/users/{id}", s.handleTelegramRevoke)
}

func (s *Server) handleTelegramStatus(w http.ResponseWriter, r *http.Request) {
	serviceHeaders(w)
	rec, ok := telegram.Secret(s.Store.Secrets())
	if !ok {
		writeJSON(w, 200, telegramStatus{})
		return
	}
	snap := s.telegramManager().Snapshot()
	out := telegramStatus{
		Connected: true, Name: rec.Name, CreatedAt: rec.CreatedAt,
		Running: snap.Running, BotID: snap.BotID, BotUsername: snap.BotUsername,
		LastPoll: snap.LastPoll, Users: snap.Users,
	}
	if snap.Claim.Code != "" {
		out.ClaimCode = snap.Claim.Code
		expires := snap.Claim.ExpiresAt
		out.ClaimExpiresAt = &expires
	}
	if !snap.Running {
		out.Warning = snap.LastError
		if out.Warning == "" {
			out.Warning = "The Telegram poller has not started yet."
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleTelegramConnect(w http.ResponseWriter, r *http.Request) {
	serviceHeaders(w)
	var in struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	token := strings.TrimSpace(in.Token)
	if !telegram.TokenPattern.MatchString(token) {
		writeJSON(w, 400, map[string]string{"error": "That does not look like a bot token. BotFather returns <bot id>:<secret>."})
		return
	}
	mctx, mcancel := context.WithTimeout(r.Context(), getMeTimeout)
	bot, err := telegram.NewClient(s.httpClient()).GetMe(mctx, token)
	mcancel()
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": telegram.ClientMessage(err)})
		return
	}
	if !bot.IsBot {
		writeJSON(w, 400, map[string]string{"error": "That token does not belong to a bot."})
		return
	}
	// Nothing is persisted until Telegram confirms the token, so a mistyped or
	// revoked token leaves no state behind.
	rec, existing := telegram.Secret(s.Store.Secrets())
	if existing {
		// Reconnecting replaces the token in place: the secret id is what the
		// dashboard's secret list and any open modal refer to.
		rec.Name = telegramSecretName
		rec.Kind = model.SecretKindTelegram
		rec.GatewayOnly = true
		rec.Value = token
		if err := s.Store.ReplaceSecret(rec); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if _, err := s.seedSecretRules(r.Context(), rec); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	} else {
		rec = store.SecretRec{
			ID: "sec_" + randID(), Name: telegramSecretName,
			Kind: model.SecretKindTelegram, GatewayOnly: true,
			Value: token, CreatedAt: time.Now().UTC(),
		}
		if _, err := s.saveSecret(r.Context(), rec); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	}
	// The cursor and the allow-list belong to a bot, not to the gateway. A
	// different bot has its own update stream, so the old cursor could skip its
	// updates, and its own people, so the old allow-list would grant strangers
	// access. Same bot keeps both: a token rotation is not a new front door.
	if link := s.Store.TelegramLink(); link.BotID != bot.ID {
		if err := s.Store.SetTelegramLink(store.TelegramLink{BotID: bot.ID}); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	}
	// Pick the new token up now rather than when the current long poll expires.
	s.telegramManager().Kick()
	writeJSON(w, 201, telegramStatus{
		Connected: true, Name: rec.Name, BotID: bot.ID,
		BotUsername: bot.Username, CreatedAt: rec.CreatedAt,
	})
}

// handleTelegramDisconnect deletes the token only. The link is kept so a
// reconnect to the same bot resumes where it left off; a different bot resets it
// in handleTelegramConnect.
func (s *Server) handleTelegramDisconnect(w http.ResponseWriter, r *http.Request) {
	serviceHeaders(w)
	rec, ok := telegram.Secret(s.Store.Secrets())
	if !ok {
		w.WriteHeader(204)
		return
	}
	if err := s.Store.DeleteSecret(rec.ID); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.telegramManager().Kick()
	w.WriteHeader(204)
}

// handleTelegramClaim mints a claim code. The code is held in memory by the
// poller, never written to disk, and replaced by the next call.
func (s *Server) handleTelegramClaim(w http.ResponseWriter, r *http.Request) {
	serviceHeaders(w)
	if _, ok := telegram.Secret(s.Store.Secrets()); !ok {
		writeJSON(w, 409, map[string]string{"error": "Connect a Telegram bot first."})
		return
	}
	claim, err := s.telegramManager().IssueClaim()
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"code": claim.Code, "expiresAt": claim.ExpiresAt})
}

func (s *Server) handleTelegramRevoke(w http.ResponseWriter, r *http.Request) {
	serviceHeaders(w)
	userID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad user id"})
		return
	}
	removed, err := s.telegramManager().Revoke(userID)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if !removed {
		writeJSON(w, 404, map[string]string{"error": "that account is not in the allow-list"})
		return
	}
	w.WriteHeader(204)
}
