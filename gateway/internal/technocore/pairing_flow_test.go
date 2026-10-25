package technocore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	tc "piso/internal/technocore"
)

// pollStub serves the one endpoint tickPending uses, so the state machine can
// be driven through every coordinator status without a real coordinator.
type pollStub struct {
	mu     sync.Mutex
	status string
	by     string
	acct   string
	dead   bool
	polls  int
}

func newPollStub(t *testing.T) (*pollStub, string) {
	t.Helper()
	st := &pollStub{status: tc.PairStatusPending, by: "alice", acct: "Alice"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.polls++
		if st.dead {
			http.Error(w, `{"error":"unknown"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "pair-1",
			"status":       st.status,
			"fingerprint":  "79541a2e2f78",
			"placement":    "local",
			"principal_id": "principal-1",
			"approved_by":  st.by,
			"account_name": st.acct,
			"expires_at":   time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(srv.Close)
	return st, srv.URL
}

func pendingIdentity(t *testing.T) (string, tc.Identity) {
	t.Helper()
	dir := t.TempDir()
	id, err := tc.Generate("https://tc.example", "")
	if err != nil {
		t.Fatal(err)
	}
	id.Name = "virginia"
	id.Status = tc.StatusPending
	id.PairingID = "pair-1"
	id.PollToken = "poll-1"
	id.PairPhase = tc.PairStatusPending
	id.PairExpiresAt = time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}
	return dir, id
}

func loadIdentity(t *testing.T, dir string) tc.Identity {
	t.Helper()
	id, err := tc.Load(tc.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The phone's tap records intent only. An identity that flips to active here
// would make the second tap decorative.
func TestTickPendingAwaitingDoesNotEnroll(t *testing.T) {
	resetLiveStatus(t)
	st, url := newPollStub(t)
	st.status = tc.PairStatusAwaitingGateway
	dir, id := pendingIdentity(t)
	id.ServerURL = url
	id.Placement = "standalone"
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}

	if err := tickPending(tc.Path(dir), id); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := loadIdentity(t, dir)
	if got.Status != tc.StatusPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}
	if got.PairPhase != tc.PairStatusAwaitingGateway {
		t.Fatalf("phase = %q", got.PairPhase)
	}
	if got.PairApprover != "alice" || got.PairAccount != "Alice" {
		t.Fatalf("approver not recorded: %+v", got)
	}
	if got.PairingID != "pair-1" {
		t.Fatalf("pairing id lost: %q", got.PairingID)
	}
	// tickPending must not write a stale whole struct over a placement the CLI
	// set in the meantime.
	if got.Placement != "standalone" {
		t.Fatalf("placement clobbered: %q", got.Placement)
	}

	snap := Snapshot(dir)
	if !snap.Awaiting || snap.Enrolled {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Status != "awaiting_gateway" {
		t.Fatalf("display status = %q", snap.Status)
	}
	if snap.PairApprover != "alice" || snap.PairAccount != "Alice" {
		t.Fatalf("snapshot approver = %+v", snap)
	}
	if snap.PairURL != url+"/pair/pair-1/" {
		t.Fatalf("pair url = %q", snap.PairURL)
	}
}

func TestTickPendingApprovedEnrolls(t *testing.T) {
	resetLiveStatus(t)
	st, url := newPollStub(t)
	st.status = tc.PairStatusApproved
	dir, id := pendingIdentity(t)
	id.ServerURL = url
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}
	if err := tickPending(tc.Path(dir), id); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := loadIdentity(t, dir)
	if got.Status != tc.StatusActive || got.PrincipalID != "principal-1" {
		t.Fatalf("identity = %+v", got)
	}
	if got.PairPhase != "" || got.PairApprover != "" || got.PairExpiresAt != "" {
		t.Fatalf("pair fields not cleared: %+v", got)
	}
	snap := Snapshot(dir)
	if !snap.Enrolled || snap.Awaiting || snap.PairURL != "" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestTickPendingDeclinedResets(t *testing.T) {
	resetLiveStatus(t)
	st, url := newPollStub(t)
	st.status = tc.PairStatusRejected
	dir, id := pendingIdentity(t)
	id.ServerURL = url
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}
	if err := tickPending(tc.Path(dir), id); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := loadIdentity(t, dir)
	if got.Status != tc.StatusNew || got.PairingID != "" || got.PollToken != "" {
		t.Fatalf("identity not reset: %+v", got)
	}
	if got.SigningPublicKey == "" || got.ServerURL != url {
		t.Fatal("reset dropped the keys or coordinator URL")
	}
	if snap := Snapshot(dir); snap.LastError == "" {
		t.Fatal("decline did not surface an error for the dashboard")
	}
}

func TestTickPendingExpiredResets(t *testing.T) {
	resetLiveStatus(t)
	st, url := newPollStub(t)
	st.status = tc.PairStatusExpired
	dir, id := pendingIdentity(t)
	id.ServerURL = url
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}
	if err := tickPending(tc.Path(dir), id); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := loadIdentity(t, dir)
	if got.Status != tc.StatusNew || got.PairingID != "" {
		t.Fatalf("identity = %+v", got)
	}
	if snap := Snapshot(dir); snap.PairURL != "" || snap.Awaiting {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestTickPendingDeadTokenResets(t *testing.T) {
	resetLiveStatus(t)
	st, url := newPollStub(t)
	st.dead = true
	dir, id := pendingIdentity(t)
	id.ServerURL = url
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}
	if err := tickPending(tc.Path(dir), id); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := loadIdentity(t, dir)
	if got.Status != tc.StatusNew || got.PairingID != "" {
		t.Fatalf("identity = %+v", got)
	}
	if snap := Snapshot(dir); snap.LastError == "" {
		t.Fatal("dead token not reported")
	}
}

// A cancel-then-mint race leaves a poll in flight for the old request. It must
// not clear the id of the attempt that replaced it.
func TestTickPendingIgnoresSupersededAttempt(t *testing.T) {
	resetLiveStatus(t)
	st, url := newPollStub(t)
	st.status = tc.PairStatusAwaitingGateway
	dir, id := pendingIdentity(t)
	newer := id
	newer.PairingID = "pair-2"
	newer.PollToken = "poll-2"
	newer.ServerURL = url
	if err := tc.Save(tc.Path(dir), newer); err != nil {
		t.Fatal(err)
	}
	stale := id
	stale.ServerURL = url
	if err := tickPending(tc.Path(dir), stale); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := loadIdentity(t, dir)
	if got.PairingID != "pair-2" || got.PairPhase != tc.PairStatusPending {
		t.Fatalf("superseded poll touched the identity: %+v", got)
	}
}
