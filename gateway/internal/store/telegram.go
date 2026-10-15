package store

import "time"

// TelegramUser is one Telegram account allowed to talk to this gateway's bot.
// It holds no credential: the gateway binds the numeric account id Telegram
// reports on every update, and that id is the whole allow-list. Username and
// FirstName are refreshed from the live update stream so the dashboard shows
// what the account calls itself now, not what it was called at claim time.
type TelegramUser struct {
	UserID    int64     `json:"userId"`
	ChatID    int64     `json:"chatId"`
	Username  string    `json:"username,omitempty"`
	FirstName string    `json:"firstName,omitempty"`
	ClaimedAt time.Time `json:"claimedAt"`
}

// TelegramLink is the gateway's Telegram front-door state. The bot token is not
// here: it is a gateway-only secret, so it never appears in the state file.
type TelegramLink struct {
	// BotID is the bot id from the last confirmed getMe. The link is keyed by
	// it: the update cursor and the allow-list belong to a bot, not to the
	// gateway, so connecting a different bot resets both.
	BotID int64 `json:"botId,omitempty"`
	// Offset is the getUpdates cursor: the next update_id to ask for. It is
	// persisted so a restart resumes the stream instead of replaying up to 24h
	// of Telegram's backlog or silently dropping what arrived meanwhile.
	Offset int64 `json:"offset,omitempty"`
	// Users is the allow-list. Empty means deny: nothing is answered until an
	// account claims the bot.
	Users []TelegramUser `json:"users,omitempty"`
}

// TelegramLink returns a copy of the stored front-door state.
func (s *Store) TelegramLink() TelegramLink {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.state.Telegram
	out.Users = append([]TelegramUser(nil), out.Users...)
	return out
}

// SetTelegramLink replaces the front-door state.
func (s *Store) SetTelegramLink(r TelegramLink) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Telegram = r
	return s.save()
}

// SetTelegramOffset advances the getUpdates cursor.
func (s *Store) SetTelegramOffset(offset int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Telegram.Offset = offset
	return s.save()
}

// SetTelegramUsers replaces the allow-list. BotID and Offset belong to the
// link, not the list, so they are preserved. When the bot is published this is
// a cache of the coordinator's copy; with no coordinator it is the truth.
func (s *Store) SetTelegramUsers(users []TelegramUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Telegram.Users = append([]TelegramUser(nil), users...)
	return s.save()
}

// TelegramAllowed reports whether the account may talk to the bot.
func (s *Store) TelegramAllowed(userID int64) (TelegramUser, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.state.Telegram.Users {
		if u.UserID == userID {
			return u, true
		}
	}
	return TelegramUser{}, false
}

// UpsertTelegramUser adds an account to the allow-list, or refreshes the stored
// name of one that is already there.
func (s *Store) UpsertTelegramUser(u TelegramUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Telegram.Users {
		if s.state.Telegram.Users[i].UserID == u.UserID {
			claimed := s.state.Telegram.Users[i].ClaimedAt
			s.state.Telegram.Users[i] = u
			// ClaimedAt is provenance: a later message must not restamp it.
			s.state.Telegram.Users[i].ClaimedAt = claimed
			return s.save()
		}
	}
	s.state.Telegram.Users = append(s.state.Telegram.Users, u)
	return s.save()
}

// RemoveTelegramUser revokes an account. It reports whether one was there.
func (s *Store) RemoveTelegramUser(userID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]TelegramUser, 0, len(s.state.Telegram.Users))
	removed := false
	for _, u := range s.state.Telegram.Users {
		if u.UserID == userID {
			removed = true
			continue
		}
		kept = append(kept, u)
	}
	if !removed {
		return false, nil
	}
	s.state.Telegram.Users = kept
	return true, s.save()
}
