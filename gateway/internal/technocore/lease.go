package technocore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	tc "piso/internal/technocore"
)

// LeaseGrant is the coordinator's answer to a poller-lease request.
type LeaseGrant struct {
	// Granted is false when another gateway holds a live lease.
	Granted bool
	// Offset is the coordinator's last known update cursor, which travels with
	// the lease so a handoff resumes instead of replaying.
	Offset int64
	// ExpiresAt bounds how long this gateway may keep polling without another
	// successful renewal.
	ExpiresAt time.Time
	// Holder is the coordinator's current holder, for diagnostics.
	Holder string
}

// TelegramLease asks the coordinator for the single-poller lease on the bot
// resource identified by resourceID, reporting this gateway's current cursor.
//
// One call both takes a free lease and renews a held one: the coordinator
// grants the first gateway that asks and the current holder, and refuses
// everyone else. ttl is the window the holder is asking for; the coordinator
// clamps it and returns the real expiry.
func TelegramLease(dataDir, resourceID string, offset int64, ttl time.Duration) (LeaseGrant, error) {
	id, err := tc.Load(tc.Path(dataDir))
	if err != nil {
		return LeaseGrant{}, err
	}
	body, err := json.Marshal(map[string]any{
		"resource_id": resourceID,
		"offset":      offset,
		"ttl":         int(ttl.Seconds()),
	})
	if err != nil {
		return LeaseGrant{}, err
	}
	code, raw, err := Call(id, http.MethodPost, "/api/v1/telegram/lease", nil, body)
	if err != nil {
		return LeaseGrant{}, err
	}
	if code != http.StatusOK {
		return LeaseGrant{}, fmt.Errorf("telegram lease: HTTP %d: %s", code, raw)
	}
	var out struct {
		Granted   bool   `json:"granted"`
		Offset    int64  `json:"offset"`
		Holder    string `json:"holder"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return LeaseGrant{}, fmt.Errorf("telegram lease: %w", err)
	}
	grant := LeaseGrant{Granted: out.Granted, Offset: out.Offset, Holder: out.Holder}
	// A missing or unparseable expiry is not fatal: the caller falls back to
	// its own requested window.
	if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
		grant.ExpiresAt = t
	}
	return grant, nil
}
