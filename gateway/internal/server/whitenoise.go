package server

// White Noise is an additive, gateway-owned transport. No connector socket,
// signing key, MLS state, or administration API is exposed to workers.
import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"piso/gateway/internal/store"
	"piso/gateway/internal/technocore"
	"piso/gateway/internal/whitenoise"
	tc "piso/internal/technocore"
)

type whiteNoiseTransport interface {
	Call(context.Context, map[string]any) (map[string]any, error)
	Subscribe(context.Context, string, func(whitenoise.Event) error) error
}
type whiteNoiseRuntime struct {
	sync.Mutex
	client whiteNoiseTransport
}

func (s *Server) wnClient() whiteNoiseTransport {
	s.whiteNoise.Lock()
	defer s.whiteNoise.Unlock()
	if s.whiteNoise.client == nil {
		s.whiteNoise.client = &whitenoise.Client{Socket: os.Getenv("PISO_WHITENOISE_SOCKET")}
	}
	return s.whiteNoise.client
}
func (s *Server) wnCoordinator(method string, body any) (int, []byte, error) {
	id, err := tc.Load(tc.Path(s.dataDir()))
	if err != nil {
		return 0, nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	return technocore.Call(id, method, "/api/v1/whitenoise", nil, raw)
}
func (s *Server) registerWhiteNoise(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/whitenoise", s.handleWhiteNoise)
	mux.HandleFunc("POST /api/v1/whitenoise", s.handleWhiteNoise)
	mux.HandleFunc("DELETE /api/v1/whitenoise", s.handleWhiteNoise)
}
func (s *Server) handleWhiteNoise(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		code, raw, err := s.wnCoordinator(http.MethodGet, nil)
		if err != nil || code != 200 {
			writeJSON(w, 502, map[string]string{"error": "shared connection status unavailable"})
			return
		}
		var out map[string]any
		if json.Unmarshal(raw, &out) != nil {
			writeJSON(w, 502, map[string]string{"error": "invalid coordinator response"})
			return
		}
		if rows, ok := out["connections"].([]any); ok {
			for _, v := range rows {
				if row, ok := v.(map[string]any); ok {
					if key, ok := row["accountId"].(string); ok {
						row["publicKey"], _ = whitenoise.PublicKey(key)
					}
					if users, ok := row["users"].([]any); ok {
						for _, v := range users {
							if u, ok := v.(map[string]any); ok {
								if key, ok := u["publicKey"].(string); ok {
									u["npub"], _ = whitenoise.PublicKey(key)
								}
							}
						}
					}
				}
			}
		}
		writeJSON(w, 200, out)
		return
	}
	var body map[string]any
	if r.Method == http.MethodPost {
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid json"})
			return
		}
	}
	if body == nil {
		body = map[string]any{}
	}
	op, _ := body["operation"].(string)
	if op == "" {
		op = "connect"
	}
	link := s.Store.WhiteNoiseLink()
	if r.Method == http.MethodDelete {
		// Fail closed locally even if the coordinator is temporarily unavailable.
		if err := s.Store.SetWhiteNoiseEnabled(false); err != nil {
			writeJSON(w, 500, map[string]string{"error": "cannot persist disconnect"})
			return
		}
	} else if op == "connect" {
		npub, _ := body["publicKey"].(string)
		key, err := whitenoise.ParsePublicKey(strings.TrimSpace(npub))
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "enter a public npub; never a private nsec"})
			return
		}
		if link.AccountID != "" && link.AccountID != key {
			writeJSON(w, 409, map[string]string{"error": "identity replacement is not supported"})
			return
		}
		out, err := s.wnClient().Call(r.Context(), map[string]any{"type": "account_list"})
		found := false
		if rows, ok := out["accounts"].([]any); ok {
			for _, v := range rows {
				if a, ok := v.(map[string]any); ok && a["account_id_hex"] == key && a["local_signing"] == true {
					found = true
				}
			}
		}
		if err != nil || !found {
			writeJSON(w, 409, map[string]string{"error": "identity not available on the gateway connector"})
			return
		}
		// Inviter authorization is distinct from per-message sender authorization.
		out, err = s.wnClient().Call(r.Context(), map[string]any{"type": "invite_policy_set", "account_id_hex": key, "policy": "allowlist"})
		if err != nil || out["type"] != "invite_policy" {
			writeJSON(w, 502, map[string]string{"error": "cannot set connector invite policy"})
			return
		}
		body = map[string]any{"operation": "connect", "accountId": key, "name": "Piso"}
	} else if op == "link" || op == "unlink" {
		npub, _ := body["publicKey"].(string)
		key, err := whitenoise.ParsePublicKey(strings.TrimSpace(npub))
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "enter a public npub; never a private nsec"})
			return
		}
		body["publicKey"] = key
		// Revoke sender access immediately, before any network work.
		if op == "unlink" {
			users := []store.WhiteNoiseUser{}
			for _, u := range link.Users {
				if u.PublicKey != key {
					users = append(users, u)
				}
			}
			if s.Store.SetWhiteNoiseUsers(users) != nil {
				writeJSON(w, 500, map[string]string{"error": "cannot persist unlink"})
				return
			}
		}
	} else {
		writeJSON(w, 400, map[string]string{"error": "unsupported operation"})
		return
	}
	code, raw, err := s.wnCoordinator(r.Method, body)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "coordinator unavailable"})
		return
	}
	if code != 200 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		w.Write(raw)
		return
	}
	if r.Method == http.MethodPost {
		var row struct {
			ResourceID string                 `json:"resourceId"`
			AccountID  string                 `json:"accountId"`
			Name       string                 `json:"name"`
			Enabled    bool                   `json:"enabled"`
			CreatedAt  time.Time              `json:"createdAt"`
			Users      []store.WhiteNoiseUser `json:"users"`
		}
		if json.Unmarshal(raw, &row) != nil || row.ResourceID == "" {
			writeJSON(w, 502, map[string]string{"error": "invalid coordinator response"})
			return
		}
		public, _ := whitenoise.PublicKey(row.AccountID)
		if err = s.Store.SetWhiteNoiseLink(store.WhiteNoiseLink{ResourceID: row.ResourceID, AccountID: row.AccountID, PublicKey: public, Name: row.Name, Enabled: row.Enabled, CreatedAt: row.CreatedAt, Users: row.Users}); err == nil {
			err = s.Store.SetWhiteNoiseUsers(row.Users)
		}
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "cannot persist connection"})
			return
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleWhiteNoiseSupervisor(w http.ResponseWriter, r *http.Request) {
	var in supervisorIn
	if json.NewDecoder(r.Body).Decode(&in) != nil || !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	if !s.workerIdentityOK(w, r, in.Worker) {
		return
	}
	rec, ok := s.Store.WorkerByName(in.Worker)
	if !ok || rec.PrincipalID == "" {
		writeJSON(w, 409, map[string]string{"error": "worker has no coordinator identity"})
		return
	}
	link := s.Store.WhiteNoiseLink()
	if !link.Enabled {
		writeJSON(w, 200, supervisorOut{Reason: "disconnected"})
		return
	}
	body := map[string]any{"operation": "supervisor", "worker_id": rec.PrincipalID}
	if link.Supervisor.Worker == in.Worker && !link.Supervisor.LastMailboxPoll.IsZero() {
		body["lastPoll"] = link.Supervisor.LastMailboxPoll
	}
	code, raw, err := s.wnCoordinator(http.MethodPost, body)
	var grant supervisorOut
	if err != nil || code != 200 || json.Unmarshal(raw, &grant) != nil {
		writeJSON(w, 502, map[string]string{"error": "coordinator unavailable"})
		return
	}
	sup := store.SupervisorLink{Worker: rec.Name, PrincipalID: rec.PrincipalID, Slug: rec.Slug, Reason: grant.Reason}
	if grant.Granted && grant.ExpiresAt != nil && grant.ExpiresAt.After(time.Now()) {
		sup.ExpiresAt = *grant.ExpiresAt
	} else {
		grant.Granted = false
	}
	if grant.Granted || link.Supervisor.Worker == rec.Name {
		if s.Store.SetWhiteNoiseSupervisor(sup) != nil {
			writeJSON(w, 500, map[string]string{"error": "cannot persist lease"})
			return
		}
	}
	writeJSON(w, 200, grant)
}
func (s *Server) handleWhiteNoiseMessages(w http.ResponseWriter, r *http.Request) {
	worker := r.URL.Query().Get("worker")
	if !workerNameRe.MatchString(worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	if !s.workerIdentityOK(w, r, worker) {
		return
	}
	wait := atoiDefault(r.URL.Query().Get("wait"), 25)
	if wait < 1 {
		wait = 1
	}
	if wait > 30 {
		wait = 30
	}
	deadline := time.NewTimer(time.Duration(wait) * time.Second)
	defer deadline.Stop()
	for {
		msgs, err := s.Store.TakeWhiteNoise(worker, time.Now())
		if err != nil {
			writeJSON(w, 409, map[string]string{"error": "not the supervisor"})
			return
		}
		if len(msgs) > 0 {
			writeJSON(w, 200, map[string]any{"messages": msgs})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			if !s.Store.WhiteNoiseSupervisor(worker) {
				writeJSON(w, 409, map[string]string{"error": "not the supervisor"})
				return
			}
			writeJSON(w, 200, map[string]any{"messages": []store.WhiteNoiseMail{}})
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}
func (s *Server) handleWhiteNoiseAck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Worker string   `json:"worker"`
		IDs    []string `json:"ids"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in) != nil || !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	if !s.workerIdentityOK(w, r, in.Worker) {
		return
	}
	if !s.Store.WhiteNoiseSupervisor(in.Worker) {
		writeJSON(w, 409, map[string]string{"error": "not the supervisor"})
		return
	}
	if s.Store.AckWhiteNoise(in.Worker, in.IDs) != nil {
		writeJSON(w, 500, map[string]string{"error": "cannot acknowledge messages"})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) handleWhiteNoiseReply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Worker       string `json:"worker"`
		Connection   string `json:"connectionId"`
		Conversation string `json:"conversationId"`
		Text         string `json:"text"`
		Key          string `json:"idempotencyKey"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&in) != nil || !workerNameRe.MatchString(in.Worker) || strings.TrimSpace(in.Text) == "" || len(in.Text) > 4096 || len(in.Key) > 128 || in.Key == "" {
		writeJSON(w, 400, map[string]string{"error": "invalid reply"})
		return
	}
	if !s.workerIdentityOK(w, r, in.Worker) {
		return
	}
	if !s.Store.WhiteNoiseSupervisor(in.Worker) {
		writeJSON(w, 409, map[string]string{"error": "not the supervisor"})
		return
	}
	if !s.Store.WhiteNoiseDestination(in.Connection, in.Conversation) {
		writeJSON(w, 403, map[string]string{"error": "conversation not authorized"})
		return
	}
	link := s.Store.WhiteNoiseLink()
	info, err := s.wnClient().Call(r.Context(), map[string]any{"type": "group_info", "account_id_hex": link.AccountID, "group_id_hex": in.Conversation})
	if err != nil || info["is_direct"] != true {
		writeJSON(w, 409, map[string]string{"error": "private conversation required"})
		return
	}
	if !s.Store.WhiteNoiseDestination(in.Connection, in.Conversation) {
		writeJSON(w, 403, map[string]string{"error": "conversation not authorized"})
		return
	}
	out, err := s.wnClient().Call(r.Context(), map[string]any{"type": "send_final", "account_id_hex": link.AccountID, "group_id_hex": in.Conversation, "text": in.Text, "reply_to_message_id_hex": nil, "idempotency_key": in.Connection + ":" + in.Conversation + ":" + in.Key})
	if err != nil || out["type"] != "final_sent" {
		writeJSON(w, 502, map[string]string{"error": "White Noise send not confirmed"})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// acceptWhiteNoise gates every message on the current sender allow-list and
// current direct-group membership. Untrusted MLS text is never configuration.
func (s *Server) acceptWhiteNoise(ctx context.Context, e whitenoise.Event) error {
	if e.Type != "inbound_message" {
		return nil
	}
	link := s.Store.WhiteNoiseLink()
	if !link.Enabled || e.Account != link.AccountID || e.Message.Sender.Self || !s.Store.WhiteNoiseAllowed(e.Message.Sender.ID) || strings.TrimSpace(e.Message.Text) == "" {
		return nil
	}
	if len(e.Message.Text) > 32768 || e.Group == "" || e.Message.ID == "" {
		return nil
	}
	info, err := s.wnClient().Call(ctx, map[string]any{"type": "group_info", "account_id_hex": e.Account, "group_id_hex": e.Group})
	if err != nil {
		return err
	}
	if info["is_direct"] != true {
		return nil
	}
	return s.Store.EnqueueWhiteNoise(store.WhiteNoiseMail{ID: e.Group + ":" + e.Message.ID, ConnectionID: link.ResourceID, ConversationID: e.Group, SenderID: e.Message.Sender.ID, Text: e.Message.Text})
}

// RunWhiteNoise uses short-lived subscriptions so disconnects, revocations and
// coordinator outages stop delivery promptly. The local allow-list is refreshed
// before subscribing; an unreachable coordinator never authorizes new delivery.
func (s *Server) RunWhiteNoise(ctx context.Context) {
	for ctx.Err() == nil {
		s.whiteNoiseCycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
func (s *Server) whiteNoiseCycle(ctx context.Context) {
	link := s.Store.WhiteNoiseLink()
	if !link.Enabled {
		return
	}
	code, raw, err := s.wnCoordinator(http.MethodGet, nil)
	var shared struct {
		Connections []struct {
			ResourceID string                 `json:"resourceId"`
			Enabled    bool                   `json:"enabled"`
			Manageable bool                   `json:"manageable"`
			Users      []store.WhiteNoiseUser `json:"users"`
		} `json:"connections"`
	}
	if err != nil || code != 200 || json.Unmarshal(raw, &shared) != nil {
		return
	}
	if len(shared.Connections) != 1 || shared.Connections[0].ResourceID != link.ResourceID || !shared.Connections[0].Enabled || !shared.Connections[0].Manageable {
		_ = s.Store.SetWhiteNoiseEnabled(false)
		return
	}
	if s.Store.SetWhiteNoiseUsers(shared.Connections[0].Users) != nil {
		return
	}
	c := s.wnClient()
	for _, u := range shared.Connections[0].Users {
		if _, err = c.Call(ctx, map[string]any{"type": "allowlist_add", "account_id_hex": link.AccountID, "welcomer_account_id_hex": u.PublicKey}); err != nil {
			return
		}
	}
	cycle, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Live events are hints; replay of known conversations closes subscription gaps.
	for _, conv := range s.Store.WhiteNoiseLink().Conversations {
		if err = s.replayWhiteNoise(cycle, link.AccountID, conv.ID); err != nil {
			return
		}
	}
	_ = c.Subscribe(cycle, link.AccountID, func(e whitenoise.Event) error {
		if e.Type == "resync_required" {
			return errors.New("resync required")
		}
		if e.Type == "ack" {
			out, err := c.Call(cycle, map[string]any{"type": "diagnostic_status", "account_id_hex": link.AccountID})
			if err != nil {
				return err
			}
			report, _ := out["report"].(map[string]any)
			relays, _ := report["relays"].(map[string]any)
			n, _ := relays["connected"].(float64)
			_, _, err = s.wnCoordinator(http.MethodPost, map[string]any{"operation": "health", "lastPoll": time.Now().UTC(), "running": n > 0})
			return err
		}
		return s.acceptWhiteNoise(cycle, e)
	})
}
func (s *Server) replayWhiteNoise(ctx context.Context, account, group string) error {
	// Page backwards to connection creation, not arbitrary historic conversation
	// content. Store dedup makes repeated recovery safe across gateway restarts.
	var before any
	for page := 0; page < 100; page++ {
		out, err := s.wnClient().Call(ctx, map[string]any{"type": "timeline_list", "account_id_hex": account, "group_id_hex": group, "limit": 100, "before": before})
		if err != nil {
			return err
		}
		rows, _ := out["messages"].([]any)
		if len(rows) == 0 {
			return nil
		}
		for _, v := range rows {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			stamp, _ := m["recorded_at"].(float64)
			if int64(stamp) < s.Store.WhiteNoiseLink().CreatedAt.Unix() {
				return nil
			}
			before = map[string]any{"recorded_at": stamp, "message_id_hex": m["message_id_hex"]}
			if m["availability"] != "available" || m["kind"] != float64(9) || m["text_truncated"] == true {
				continue
			}
			raw, _ := json.Marshal(map[string]any{"type": "inbound_message", "account_id_hex": account, "group_id_hex": group, "message": m})
			var e whitenoise.Event
			_ = json.Unmarshal(raw, &e)
			if err = s.acceptWhiteNoise(ctx, e); err != nil {
				return err
			}
		}
		if out["has_more_before"] != true {
			return nil
		}
	}
	return errors.New("White Noise recovery backlog exceeds page limit")
}
