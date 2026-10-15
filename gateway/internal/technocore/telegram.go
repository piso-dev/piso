package technocore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

// TelegramDoorClaim is the coordinator's pending claim code for a bot.
type TelegramDoorClaim struct {
	Code      string
	ExpiresAt time.Time
}

// TelegramDoorState is the coordinator's allow-list and pending claim.
type TelegramDoorState struct {
	Users []store.TelegramUser
	Claim *TelegramDoorClaim
}

type telegramClaimWire struct {
	Code      string `json:"code"`
	ExpiresAt string `json:"expires_at"`
}

type telegramStateWire struct {
	Users []store.TelegramUser `json:"users"`
	Claim *telegramClaimWire   `json:"claim"`
}

type telegramUsersWire struct {
	Allowed bool                 `json:"allowed"`
	Users   []store.TelegramUser `json:"users"`
}

// telegramBotPath is a coordinator path for one bot. The resource id rides in
// the path rather than a query because the gateway signature covers the path,
// and a query would not be signed.
func telegramBotPath(resourceID, suffix string) string {
	return "/api/v1/telegram/bots/" + url.PathEscape(resourceID) + suffix
}

// telegramCall is a signed front-door request. in may be nil. The raw status is
// returned so a caller can read a 404 as "no such account" rather than a fault.
func telegramCall(dataDir, method, path string, in any) (int, []byte, error) {
	id, err := tc.Load(tc.Path(dataDir))
	if err != nil {
		return 0, nil, err
	}
	var body []byte
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return 0, nil, err
		}
	}
	return Call(id, method, path, nil, body)
}

func telegramCallOK(dataDir, method, path string, in, out any) error {
	status, raw, err := telegramCall(dataDir, method, path, in)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("telegram front door: HTTP %d: %s", status, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func telegramClaimFromWire(w *telegramClaimWire) *TelegramDoorClaim {
	if w == nil || w.Code == "" {
		return nil
	}
	out := &TelegramDoorClaim{Code: w.Code}
	if t, err := time.Parse(time.RFC3339, w.ExpiresAt); err == nil {
		out.ExpiresAt = t
	}
	return out
}

// TelegramFrontDoor reads a bot's allow-list and pending claim from the
// coordinator. The coordinator owns them because the token reaches every
// enrolled gateway, so membership cannot live on just one.
func TelegramFrontDoor(dataDir, resourceID string) (TelegramDoorState, error) {
	var wire telegramStateWire
	if err := telegramCallOK(dataDir, http.MethodGet, telegramBotPath(resourceID, "/front-door"), nil, &wire); err != nil {
		return TelegramDoorState{}, err
	}
	return TelegramDoorState{Users: wire.Users, Claim: telegramClaimFromWire(wire.Claim)}, nil
}

// TelegramIssueClaim mints a claim code, replacing any outstanding one.
func TelegramIssueClaim(dataDir, resourceID string) (TelegramDoorClaim, error) {
	var wire telegramClaimWire
	if err := telegramCallOK(dataDir, http.MethodPost, telegramBotPath(resourceID, "/claim"), map[string]any{}, &wire); err != nil {
		return TelegramDoorClaim{}, err
	}
	claim := TelegramDoorClaim{Code: wire.Code}
	if t, err := time.Parse(time.RFC3339, wire.ExpiresAt); err == nil {
		claim.ExpiresAt = t
	}
	return claim, nil
}

// TelegramRedeemClaim consumes a claim code for the sending account.
// allowed=false is an answer, not a fault: it is what a wrong or expired code
// gets.
func TelegramRedeemClaim(dataDir, resourceID, code string, u store.TelegramUser) (bool, []store.TelegramUser, error) {
	var wire telegramUsersWire
	body := map[string]any{"code": code, "user": u}
	if err := telegramCallOK(dataDir, http.MethodPost, telegramBotPath(resourceID, "/claim/redeem"), body, &wire); err != nil {
		return false, nil, err
	}
	return wire.Allowed, wire.Users, nil
}

// TelegramUpsertUser refreshes an account's stored name. It cannot add one.
func TelegramUpsertUser(dataDir, resourceID string, u store.TelegramUser) ([]store.TelegramUser, error) {
	var wire telegramUsersWire
	path := telegramBotPath(resourceID, "/users/"+strconv.FormatInt(u.UserID, 10))
	body := map[string]any{"username": u.Username, "firstName": u.FirstName}
	if err := telegramCallOK(dataDir, http.MethodPut, path, body, &wire); err != nil {
		return nil, err
	}
	return wire.Users, nil
}

// TelegramRevokeUser removes an account. found=false with a nil error means the
// coordinator had already forgotten it, which is not a failure.
func TelegramRevokeUser(dataDir, resourceID string, userID int64) (bool, []store.TelegramUser, error) {
	path := telegramBotPath(resourceID, "/users/"+strconv.FormatInt(userID, 10))
	status, raw, err := telegramCall(dataDir, http.MethodDelete, path, nil)
	if err != nil {
		return false, nil, err
	}
	if status == http.StatusNotFound {
		return false, nil, nil
	}
	if status != http.StatusOK {
		return false, nil, fmt.Errorf("telegram front door: HTTP %d: %s", status, raw)
	}
	var wire telegramUsersWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return false, nil, err
	}
	return true, wire.Users, nil
}
