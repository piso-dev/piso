package technocore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// pairingStub stands in for the coordinator's pairing API. The two-phase flow
// lives on both sides of the wire, so the client half is tested against a
// stub that behaves like the real endpoints rather than a fake transport.
type pairingStub struct {
	mu           sync.Mutex
	creates      int
	status       string
	approvedBy   string
	accountName  string
	principalID  string
	rejectToken  bool
	rateLimited  bool
	confirmCalls int
	declineCalls int
	lastCreate   map[string]any
	expiresAt    string
}

func newPairingStub(t *testing.T) (*pairingStub, *httptest.Server) {
	t.Helper()
	st := &pairingStub{
		status:      PairStatusPending,
		principalID: "principal-1",
		expiresAt:   time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pairing-requests", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.rateLimited {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		st.creates++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		st.lastCreate = body
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         "pair-1",
			"poll_token": "poll-1",
			"expires_at": st.expiresAt,
		})
	})
	mux.HandleFunc("/api/v1/pairing-requests/pair-1", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.rejectToken || r.Header.Get(HeaderPollToken) != "poll-1" {
			http.Error(w, `{"error":"unknown"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "pair-1",
			"status":       st.status,
			"fingerprint":  "79541a2e2f78",
			"placement":    "local",
			"principal_id": st.principalID,
			"approved_by":  st.approvedBy,
			"account_name": st.accountName,
			"expires_at":   st.expiresAt,
		})
	})
	mux.HandleFunc("/api/v1/pairing-requests/pair-1/confirm", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.confirmCalls++
		if st.rejectToken || r.Header.Get(HeaderPollToken) != "poll-1" {
			http.Error(w, `{"error":"unknown"}`, http.StatusUnauthorized)
			return
		}
		if st.status != PairStatusAwaitingGateway {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "not waiting for the gateway"})
			return
		}
		st.status = PairStatusApproved
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":       PairStatusApproved,
			"principal_id": st.principalID,
		})
	})
	mux.HandleFunc("/api/v1/pairing-requests/pair-1/decline", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.declineCalls++
		st.status = PairStatusRejected
		_ = json.NewEncoder(w).Encode(map[string]any{"status": PairStatusRejected})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return st, srv
}

func pairingIdentity(t *testing.T, dir, serverURL string) Identity {
	t.Helper()
	id, err := Generate(serverURL, "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	id.Name = "virginia"
	id.ServerURL = serverURL
	if err := Save(Path(dir), id); err != nil {
		t.Fatalf("save: %v", err)
	}
	return id
}

// A dashboard mint must not overwrite fields the CLI owns: it writes the whole
// identity file, and the CLI may have set a placement moments earlier.
func TestStartPairingTouchesOnlyPairingFields(t *testing.T) {
	st, srv := newPairingStub(t)
	dir := t.TempDir()
	id := pairingIdentity(t, dir, srv.URL)
	id.Placement = "standalone"
	if err := Save(Path(dir), id); err != nil {
		t.Fatal(err)
	}

	pending, err := StartPairing(Path(dir), id)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if pending.PairingID != "pair-1" || pending.PollToken != "poll-1" {
		t.Fatalf("pairing fields not set: %+v", pending)
	}
	if pending.Status != StatusPending || pending.PairPhase != PairStatusPending {
		t.Fatalf("status = %q phase = %q", pending.Status, pending.PairPhase)
	}
	if pending.Placement != "standalone" {
		t.Fatalf("placement clobbered: %q", pending.Placement)
	}
	if pending.Name != "virginia" || pending.ServerURL != srv.URL {
		t.Fatalf("identity fields not preserved: %+v", pending)
	}

	loaded, err := Load(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Placement != "standalone" || loaded.PairingID != "pair-1" {
		t.Fatalf("saved identity = %+v", loaded)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.creates != 1 {
		t.Fatalf("creates = %d, want 1", st.creates)
	}
	if st.lastCreate["kind"] != "gateway" || st.lastCreate["placement"] != "standalone" {
		t.Fatalf("create body = %+v", st.lastCreate)
	}
	if st.lastCreate["device_pubkey"] != id.SigningPublicKey {
		t.Fatal("device pubkey not sent")
	}
}

// Minting twice would burn the per-IP creation rate limit and leave the phone
// looking at a QR the gateway no longer polls.
func TestStartPairingReusesLiveRequest(t *testing.T) {
	st, srv := newPairingStub(t)
	dir := t.TempDir()
	id := pairingIdentity(t, dir, srv.URL)

	first, err := StartPairing(Path(dir), id)
	if err != nil {
		t.Fatal(err)
	}
	second, err := StartPairing(Path(dir), first)
	if err != nil {
		t.Fatal(err)
	}
	if second.PairingID != first.PairingID || second.PollToken != first.PollToken {
		t.Fatalf("second mint = %+v", second)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.creates != 1 {
		t.Fatalf("creates = %d, want 1", st.creates)
	}
}

func TestStartPairingMintsAgainAfterExpiry(t *testing.T) {
	st, srv := newPairingStub(t)
	dir := t.TempDir()
	id := pairingIdentity(t, dir, srv.URL)
	id.PairingID = "pair-1"
	id.PollToken = "poll-1"
	id.Status = StatusPending
	id.PairExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := Save(Path(dir), id); err != nil {
		t.Fatal(err)
	}
	if _, err := StartPairing(Path(dir), id); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.creates != 1 {
		t.Fatalf("creates = %d, want 1", st.creates)
	}
}

func TestStartPairingSurfacesRateLimit(t *testing.T) {
	st, srv := newPairingStub(t)
	st.rateLimited = true
	dir := t.TempDir()
	id := pairingIdentity(t, dir, srv.URL)
	_, err := StartPairing(Path(dir), id)
	if err == nil || !strings.Contains(err.Error(), "too many pairing requests") {
		t.Fatalf("err = %v", err)
	}
}

// The dashboard needs the approver's name the moment the phone taps, before
// anything is minted.
func TestPollPairingReportsAwaitingApprover(t *testing.T) {
	st, srv := newPairingStub(t)
	dir := t.TempDir()
	id := pairingIdentity(t, dir, srv.URL)
	if _, err := StartPairing(Path(dir), id); err != nil {
		t.Fatal(err)
	}
	id = idSnapshot(t, dir)
	st.mu.Lock()
	st.status = PairStatusAwaitingGateway
	st.approvedBy = "alice"
	st.accountName = "Alice"
	st.mu.Unlock()

	state, err := PollPairing(id)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !state.AwaitingConfirm() {
		t.Fatalf("state = %+v", state)
	}
	if state.ApprovedBy != "alice" || state.AccountName != "Alice" {
		t.Fatalf("approver not reported: %+v", state)
	}
	if state.Fingerprint != "79541a2e2f78" {
		t.Fatalf("fingerprint = %q", state.Fingerprint)
	}
}

func TestPollPairingReportsDeadToken(t *testing.T) {
	st, srv := newPairingStub(t)
	dir := t.TempDir()
	id := pairingIdentity(t, dir, srv.URL)
	if _, err := StartPairing(Path(dir), id); err != nil {
		t.Fatal(err)
	}
	id = idSnapshot(t, dir)
	st.mu.Lock()
	st.rejectToken = true
	st.mu.Unlock()
	if _, err := PollPairing(id); err != ErrPollToken {
		t.Fatalf("err = %v, want ErrPollToken", err)
	}
}

func TestConfirmPairingReturnsPrincipal(t *testing.T) {
	st, srv := newPairingStub(t)
	dir := t.TempDir()
	id := pairingIdentity(t, dir, srv.URL)
	if _, err := StartPairing(Path(dir), id); err != nil {
		t.Fatal(err)
	}
	id = idSnapshot(t, dir)
	st.mu.Lock()
	st.status = PairStatusAwaitingGateway
	st.mu.Unlock()

	principal, err := ConfirmPairing(id)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if principal != "principal-1" {
		t.Fatalf("principal = %q", principal)
	}

	// Confirming a request that is not waiting is a programming error, not a
	// silent success.
	if _, err := ConfirmPairing(id); err == nil || !strings.Contains(err.Error(), "not waiting") {
		t.Fatalf("second confirm err = %v", err)
	}
}

func TestDeclinePairing(t *testing.T) {
	st, srv := newPairingStub(t)
	dir := t.TempDir()
	id := pairingIdentity(t, dir, srv.URL)
	if _, err := StartPairing(Path(dir), id); err != nil {
		t.Fatal(err)
	}
	id = idSnapshot(t, dir)
	if err := DeclinePairing(id); err != nil {
		t.Fatalf("decline: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.declineCalls != 1 {
		t.Fatalf("decline calls = %d", st.declineCalls)
	}
}

func TestPairURLEscapesNothing(t *testing.T) {
	id := Identity{ServerURL: "https://piso.dev/", PairingID: "abc"}
	if got := PairURL(id); got != "https://piso.dev/pair/abc/" {
		t.Fatalf("PairURL = %q", got)
	}
	if got := PairURL(Identity{}); got != "" {
		t.Fatalf("PairURL without a request = %q", got)
	}
}

func TestClearPairingRefusesWhenActive(t *testing.T) {
	dir := t.TempDir()
	id := pairingIdentity(t, dir, "https://piso.dev")
	id.Status = StatusActive
	id.PairingID = "abc"
	if err := Save(Path(dir), id); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearPairing(Path(dir)); err == nil {
		t.Fatal("cleared an active identity")
	}
	id.Status = StatusPending
	if err := Save(Path(dir), id); err != nil {
		t.Fatal(err)
	}
	cleared, err := ClearPairing(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Status != StatusNew || cleared.PairingID != "" || cleared.PollToken != "" {
		t.Fatalf("cleared = %+v", cleared)
	}
}

func idSnapshot(t *testing.T, dir string) Identity {
	t.Helper()
	id, err := Load(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	return id
}
