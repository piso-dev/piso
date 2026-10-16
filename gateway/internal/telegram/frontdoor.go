package telegram

import (
	"crypto/subtle"
	"errors"
	"sync"
	"time"

	"piso/gateway/internal/store"
	"piso/gateway/internal/technocore"
)

// Claim is a pending claim code, held by whoever owns the front door.
type Claim struct {
	Code      string    `json:"code,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

// FrontDoorState is the allow-list plus any pending claim.
type FrontDoorState struct {
	Users []store.TelegramUser
	Claim Claim
}

// FrontDoor is the allow-list and claim handshake for one bot. The coordinator
// implements it once the token is published, because the token reaches every
// enrolled gateway and membership cannot live on just one; a gateway with no
// coordinator falls back to its own store.
type FrontDoor interface {
	// Refresh pulls the authoritative allow-list into the local cache. It is a
	// no-op when the store is already the authority.
	Refresh() error
	// State returns the allow-list and any pending claim.
	State() (FrontDoorState, error)
	// Issue mints a claim code, replacing any outstanding one.
	Issue() (Claim, error)
	// Redeem consumes a claim code for the sending account. The returned state
	// is the allow-list after the bind.
	Redeem(code string, u store.TelegramUser) (bool, FrontDoorState, error)
	// Upsert refreshes an account's stored name. It never adds an account.
	Upsert(u store.TelegramUser) error
	// Revoke removes an account, reporting whether one was there.
	Revoke(userID int64) (bool, error)
}

// localFrontDoor is the front door of a gateway with no coordinator: the store
// is the authority and the claim code lives only in memory.
type localFrontDoor struct {
	st  *store.Store
	now func() time.Time

	mu    sync.Mutex
	claim claimState
}

type claimState struct {
	Code      string
	ExpiresAt time.Time
	Attempts  int
}

// NewLocalFrontDoor returns the store-backed front door.
func NewLocalFrontDoor(st *store.Store, now func() time.Time) FrontDoor {
	if now == nil {
		now = time.Now
	}
	return &localFrontDoor{st: st, now: now}
}

func (f *localFrontDoor) Refresh() error { return nil }

func (f *localFrontDoor) State() (FrontDoorState, error) { return f.state(), nil }

func (f *localFrontDoor) state() FrontDoorState {
	out := FrontDoorState{Users: f.st.TelegramLink().Users}
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claim.Code != "" && now.Before(f.claim.ExpiresAt) {
		out.Claim = Claim{Code: f.claim.Code, ExpiresAt: f.claim.ExpiresAt}
	}
	return out
}

func (f *localFrontDoor) Issue() (Claim, error) {
	code, err := randomCode()
	if err != nil {
		return Claim{}, err
	}
	out := Claim{Code: code, ExpiresAt: f.now().Add(ClaimTTL)}
	f.mu.Lock()
	f.claim = claimState{Code: code, ExpiresAt: out.ExpiresAt}
	f.mu.Unlock()
	return out, nil
}

// Redeem is the local claim handshake. The code is the only thing between a
// stranger who found the bot and the agent, so the comparison is constant time
// and a short run of wrong guesses burns the code.
func (f *localFrontDoor) Redeem(code string, u store.TelegramUser) (bool, FrontDoorState, error) {
	now := f.now()
	f.mu.Lock()
	pending := f.claim
	live := pending.Code != "" && now.Before(pending.ExpiresAt)
	match := live && subtle.ConstantTimeCompare([]byte(code), []byte(pending.Code)) == 1
	switch {
	case match:
		f.claim = claimState{}
	case live:
		pending.Attempts++
		if pending.Attempts >= claimAttempts {
			f.claim = claimState{}
		} else {
			f.claim = pending
		}
	}
	f.mu.Unlock()
	if !match {
		return false, f.state(), nil
	}
	u.ClaimedAt = now
	if err := f.st.UpsertTelegramUser(u); err != nil {
		return false, f.state(), err
	}
	return true, f.state(), nil
}

func (f *localFrontDoor) Upsert(u store.TelegramUser) error {
	return f.st.UpsertTelegramUser(u)
}

func (f *localFrontDoor) Revoke(userID int64) (bool, error) {
	return f.st.RemoveTelegramUser(userID)
}

// frontDoorAPI is the coordinator's front-door endpoints as this package needs
// them. Production is technocore; a test stubs it.
type frontDoorAPI interface {
	State(resourceID string) (FrontDoorState, error)
	Issue(resourceID string) (Claim, error)
	Redeem(resourceID, code string, u store.TelegramUser) (bool, []store.TelegramUser, error)
	Upsert(resourceID string, u store.TelegramUser) ([]store.TelegramUser, error)
	Revoke(resourceID string, userID int64) (bool, []store.TelegramUser, error)
}

// remoteFrontDoor is the front door once the bot is published to a coordinator:
// the coordinator is the authority, and the store is only a cache of the
// allow-list for the message hot path.
type remoteFrontDoor struct {
	st  *store.Store
	api frontDoorAPI
}

// newRemoteFrontDoor returns the coordinator-backed front door.
func newRemoteFrontDoor(st *store.Store, api frontDoorAPI) FrontDoor {
	return &remoteFrontDoor{st: st, api: api}
}

func (f *remoteFrontDoor) resourceID() (string, error) {
	rec, ok := Secret(f.st.Secrets())
	if !ok || rec.TechnocoreID == "" {
		return "", errors.New("telegram bot is not published")
	}
	return rec.TechnocoreID, nil
}

func (f *remoteFrontDoor) Refresh() error {
	id, err := f.resourceID()
	if err != nil {
		return err
	}
	state, err := f.api.State(id)
	if err != nil {
		return err
	}
	return f.st.SetTelegramUsers(state.Users)
}

func (f *remoteFrontDoor) State() (FrontDoorState, error) {
	id, err := f.resourceID()
	if err != nil {
		return FrontDoorState{}, err
	}
	return f.api.State(id)
}

func (f *remoteFrontDoor) Issue() (Claim, error) {
	id, err := f.resourceID()
	if err != nil {
		return Claim{}, err
	}
	return f.api.Issue(id)
}

func (f *remoteFrontDoor) Redeem(code string, u store.TelegramUser) (bool, FrontDoorState, error) {
	id, err := f.resourceID()
	if err != nil {
		return false, FrontDoorState{}, err
	}
	allowed, users, err := f.api.Redeem(id, code, u)
	if err != nil {
		return false, FrontDoorState{}, err
	}
	if allowed {
		// Cache it now rather than waiting for the coordinator's push: the new
		// account must be able to talk as soon as it is told it is connected.
		if err := f.st.SetTelegramUsers(users); err != nil {
			return false, FrontDoorState{}, err
		}
	}
	return allowed, FrontDoorState{Users: users}, nil
}

func (f *remoteFrontDoor) Upsert(u store.TelegramUser) error {
	id, err := f.resourceID()
	if err != nil {
		return err
	}
	users, err := f.api.Upsert(id, u)
	if err != nil {
		return err
	}
	return f.st.SetTelegramUsers(users)
}

func (f *remoteFrontDoor) Revoke(userID int64) (bool, error) {
	id, err := f.resourceID()
	if err != nil {
		return false, err
	}
	found, users, err := f.api.Revoke(id, userID)
	if err != nil {
		return false, err
	}
	if users != nil {
		if err := f.st.SetTelegramUsers(users); err != nil {
			return found, err
		}
	}
	return found, nil
}

// coordinatorFrontDoor adapts the technocore client to frontDoorAPI. Keeping
// the mapping here is what lets the rest of the package work in store types.
type coordinatorFrontDoor struct {
	dataDir string
}

// NewCoordinatorFrontDoor returns a front door backed by the account's
// coordinator, which is the authority once the bot has been published.
func NewCoordinatorFrontDoor(st *store.Store, dataDir string) FrontDoor {
	return newRemoteFrontDoor(st, coordinatorFrontDoor{dataDir: dataDir})
}

func (c coordinatorFrontDoor) State(resourceID string) (FrontDoorState, error) {
	state, err := technocore.TelegramFrontDoor(c.dataDir, resourceID)
	if err != nil {
		return FrontDoorState{}, err
	}
	out := FrontDoorState{Users: state.Users}
	if state.Claim != nil {
		out.Claim = Claim{Code: state.Claim.Code, ExpiresAt: state.Claim.ExpiresAt}
	}
	return out, nil
}

func (c coordinatorFrontDoor) Issue(resourceID string) (Claim, error) {
	claim, err := technocore.TelegramIssueClaim(c.dataDir, resourceID)
	if err != nil {
		return Claim{}, err
	}
	return Claim{Code: claim.Code, ExpiresAt: claim.ExpiresAt}, nil
}

func (c coordinatorFrontDoor) Redeem(resourceID, code string, u store.TelegramUser) (bool, []store.TelegramUser, error) {
	return technocore.TelegramRedeemClaim(c.dataDir, resourceID, code, u)
}

func (c coordinatorFrontDoor) Upsert(resourceID string, u store.TelegramUser) ([]store.TelegramUser, error) {
	return technocore.TelegramUpsertUser(c.dataDir, resourceID, u)
}

func (c coordinatorFrontDoor) Revoke(resourceID string, userID int64) (bool, []store.TelegramUser, error) {
	return technocore.TelegramRevokeUser(c.dataDir, resourceID, userID)
}
