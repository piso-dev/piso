package technocore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Pairing has two phases. The phone only *approves* (records intent); the
// gateway's own dashboard confirm is what mints the Principal and lets the
// existing gateways wrap their vaults for the new recipient. Enrolling on the
// first tap would hand a screenshot of a QR the whole vault.
const (
	PairStatusPending         = "pending"
	PairStatusAwaitingGateway = "awaiting_gateway"
	PairStatusApproved        = "approved"
	PairStatusRejected        = "rejected"
	PairStatusExpired         = "expired"
)

// NeedsLoginError is returned when the coordinator will not finish an
// enrollment for a poll token alone. That happens when the phone approved
// without signing in: request creation is unauthenticated, so the poll token
// proves nothing about who approved. A signed-in confirmation on the
// coordinator is the missing step.
type NeedsLoginError struct {
	// ConfirmURL is the coordinator path that finishes this enrollment
	// (/pair/<uuid>/), relative to the coordinator origin.
	ConfirmURL string
}

func (e *NeedsLoginError) Error() string {
	return "finish this enrollment from a signed-in browser"
}

// ErrPollToken is returned when the coordinator no longer recognises this
// gateway's poll token (unknown request, consumed pairing, or a revoked id).
var ErrPollToken = errors.New("pairing token rejected by the coordinator")

// PairState is the coordinator's view of one pairing request.
type PairState struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Fingerprint string `json:"fingerprint"`
	Placement   string `json:"placement"`
	PrincipalID string `json:"principal_id"`
	ApprovedBy  string `json:"approved_by"`
	AccountName string `json:"account_name"`
	ExpiresAt   string `json:"expires_at"`
	WorkerToken string `json:"worker_token"`
}

// AwaitingConfirm reports whether a phone has approved and the gateway still
// has to confirm before the enrollment takes effect.
func (p PairState) AwaitingConfirm() bool {
	return p.Status == PairStatusAwaitingGateway
}

var pairingHTTP = &http.Client{Timeout: 20 * time.Second}

// StartPairing asks the coordinator for a pending request and stores the
// poll token on the identity. It is safe to call twice: a live pending
// request is reused instead of minting a second one, which matters because
// the coordinator rate-limits request creation per IP.
func StartPairing(path string, id Identity) (Identity, error) {
	if strings.TrimSpace(id.ServerURL) == "" {
		return Identity{}, errors.New("no coordinator URL")
	}
	if id.PairingID != "" && id.Status == StatusPending && !pairingExpired(id.PairExpiresAt) {
		return id, nil
	}
	body, err := json.Marshal(map[string]any{
		"kind":              "gateway",
		"device_pubkey":     id.SigningPublicKey,
		"encryption_pubkey": id.EncryptionPublicKey,
		"name":              id.Name,
		// A dashboard enrollment is always a laptop/desktop gateway. The
		// standalone placement stays a CLI flag (`piso gateway launch
		// --standalone`) because it decides whether the machine is a worker
		// host, and that is not a decision to make by mis-tapping a radio.
		"placement":    id.PlacementOrLocal(),
		"capabilities": []string{"credential-relay"},
	})
	if err != nil {
		return Identity{}, err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(id.ServerURL, "/")+"/api/v1/pairing-requests", bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := pairingHTTP.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("pairing: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusTooManyRequests {
		return Identity{}, errors.New("too many pairing requests from this gateway; wait a few minutes and try again")
	}
	if res.StatusCode != http.StatusCreated {
		return Identity{}, fmt.Errorf("pairing HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		ID        string `json:"id"`
		PollToken string `json:"poll_token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Identity{}, err
	}
	if out.ID == "" || out.PollToken == "" {
		return Identity{}, errors.New("pairing response missing id or token")
	}
	return Update(path, func(cur *Identity) error {
		// Touch only the pairing fields: a whole-struct save would clobber a
		// placement/public-URL write the CLI made a moment ago.
		cur.ServerURL = strings.TrimRight(id.ServerURL, "/")
		cur.Name = id.Name
		cur.PairingID = out.ID
		cur.PollToken = out.PollToken
		cur.Status = StatusPending
		cur.PairPhase = PairStatusPending
		cur.PairExpiresAt = out.ExpiresAt
		cur.PairApprover = ""
		cur.PairAccount = ""
		return nil
	})
}

// PollPairing asks the coordinator for the request's current state. A 401
// means the token is no longer usable (unknown request or a superseded
// pairing), which callers surface as "not recognised" rather than retrying.
func PollPairing(id Identity) (PairState, error) {
	if id.PairingID == "" {
		return PairState{}, errors.New("no pairing request")
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(id.ServerURL, "/")+"/api/v1/pairing-requests/"+id.PairingID, nil)
	if err != nil {
		return PairState{}, err
	}
	req.Header.Set(HeaderPollToken, id.PollToken)
	res, err := pairingHTTP.Do(req)
	if err != nil {
		return PairState{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusNotFound {
		return PairState{}, ErrPollToken
	}
	if res.StatusCode != http.StatusOK {
		return PairState{}, fmt.Errorf("pairing poll HTTP %d", res.StatusCode)
	}
	var out PairState
	if err := json.Unmarshal(raw, &out); err != nil {
		return PairState{}, err
	}
	return out, nil
}

// ConfirmPairing is the gateway-side second tap. It returns the principal id
// the coordinator minted.
func ConfirmPairing(id Identity) (string, error) {
	var out struct {
		Status      string `json:"status"`
		PrincipalID string `json:"principal_id"`
	}
	if err := pairingAction(id, "confirm", &out); err != nil {
		return "", err
	}
	return out.PrincipalID, nil
}

// DeclinePairing rejects a request that a phone approved by mistake.
func DeclinePairing(id Identity) error {
	return pairingAction(id, "decline", nil)
}

func pairingAction(id Identity, action string, out any) error {
	if id.PairingID == "" {
		return errors.New("no pairing request")
	}
	u := strings.TrimRight(id.ServerURL, "/") + "/api/v1/pairing-requests/" + id.PairingID + "/" + action
	req, err := http.NewRequest(http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set(HeaderPollToken, id.PollToken)
	res, err := pairingHTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusUnauthorized {
		return ErrPollToken
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error      string `json:"error"`
			NeedsLogin bool   `json:"needs_login"`
			ConfirmURL string `json:"confirm_url"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.NeedsLogin {
			return &NeedsLoginError{ConfirmURL: e.ConfirmURL}
		}
		if e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s HTTP %d", action, res.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// PairURL is the page a phone opens (or scans).
func PairURL(id Identity) string {
	if id.PairingID == "" || id.ServerURL == "" {
		return ""
	}
	return strings.TrimRight(id.ServerURL, "/") + "/pair/" + id.PairingID + "/"
}

// ClearPairing drops the local pairing attempt and returns to an unpaired
// identity. The coordinator request is left to expire.
func ClearPairing(path string) (Identity, error) {
	return Update(path, func(cur *Identity) error {
		if cur.Status == StatusActive {
			return errors.New("gateway is already paired")
		}
		cur.Status = StatusNew
		cur.PairingID = ""
		cur.PollToken = ""
		cur.PairPhase = ""
		cur.PairExpiresAt = ""
		cur.PairApprover = ""
		cur.PairAccount = ""
		return nil
	})
}

func pairingExpired(ts string) bool {
	if strings.TrimSpace(ts) == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return time.Now().After(t)
}
