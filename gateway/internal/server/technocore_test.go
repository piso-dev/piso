package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

// coordStub is a coordinator's pairing API plus /health/. The dashboard's
// enroll path is judged by what it sends and by what it refuses to send, so
// the stub records every call.
type coordStub struct {
	mu            sync.Mutex
	healthStatus  int
	creates       int
	lastCreate    map[string]any
	status        string
	approvedBy    string
	accountName   string
	confirmLogin  bool
	confirmCalled int
	declineCalled int
}

func newCoordStub(t *testing.T) (*coordStub, string) {
	t.Helper()
	st := &coordStub{status: tc.PairStatusPending, approvedBy: "alice", accountName: "Alice"}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		code := st.healthStatus
		st.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			_, _ = w.Write([]byte("nope"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/api/v1/pairing-requests", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.creates++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		st.lastCreate = body
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         "pair-1",
			"poll_token": "poll-1",
			"expires_at": time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/api/v1/pairing-requests/pair-1", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "pair-1",
			"status":       st.status,
			"fingerprint":  "79541a2e2f78",
			"placement":    "local",
			"approved_by":  st.approvedBy,
			"account_name": st.accountName,
			"expires_at":   time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/api/v1/pairing-requests/pair-1/confirm", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.confirmCalled++
		if st.confirmLogin {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":       "finish this enrollment from a signed-in browser",
				"needs_login": true,
				"confirm_url": "/pair/pair-1/",
			})
			return
		}
		if st.status != tc.PairStatusAwaitingGateway {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not waiting for the gateway"})
			return
		}
		st.status = tc.PairStatusApproved
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":       tc.PairStatusApproved,
			"principal_id": "principal-1",
		})
	})
	mux.HandleFunc("/api/v1/pairing-requests/pair-1/decline", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.declineCalled++
		st.status = tc.PairStatusRejected
		_ = json.NewEncoder(w).Encode(map[string]string{"status": tc.PairStatusRejected})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return st, srv.URL
}

func technocoreServer(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	s.DataDir = t.TempDir()
	return s
}

func seedIdentity(t *testing.T, dir, serverURL string, mut func(*tc.Identity)) tc.Identity {
	t.Helper()
	id, err := tc.Generate(serverURL, "")
	if err != nil {
		t.Fatal(err)
	}
	id.Name = "virginia"
	id.ServerURL = serverURL
	if mut != nil {
		mut(&id)
	}
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func loadTestIdentity(t *testing.T, dir string) tc.Identity {
	t.Helper()
	id, err := tc.Load(tc.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return out
}

func TestPairMintsAndReusesOnSecondClick(t *testing.T) {
	st, coord := newCoordStub(t)
	s := technocoreServer(t)
	h := s.ControlHandler()

	first := doJSON(t, h, "POST", "/api/v1/technocore/pair", map[string]any{
		"serverUrl": coord, "name": "virginia",
	})
	if first.Code != 200 {
		t.Fatalf("pair = %d %s", first.Code, first.Body)
	}
	body := decodeBody(t, first)
	pairURL, _ := body["pairUrl"].(string)
	if !strings.HasPrefix(pairURL, coord+"/pair/") {
		t.Fatalf("pairUrl = %q", pairURL)
	}
	if body["awaitingConfirm"] != false {
		t.Fatalf("awaitingConfirm = %v", body["awaitingConfirm"])
	}
	if id := loadTestIdentity(t, s.DataDir); id.Status != tc.StatusPending || id.PairingID != "pair-1" {
		t.Fatalf("identity = %+v", id)
	}

	second := doJSON(t, h, "POST", "/api/v1/technocore/pair", map[string]any{"serverUrl": coord})
	if second.Code != 200 {
		t.Fatalf("second pair = %d %s", second.Code, second.Body)
	}
	if got, _ := decodeBody(t, second)["pairUrl"].(string); got != pairURL {
		t.Fatalf("pairUrl changed to %q", got)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.creates != 1 {
		t.Fatalf("creates = %d, want 1", st.creates)
	}
	if st.lastCreate["placement"] != tc.PlacementLocal {
		t.Fatalf("dashboard enrollment sent placement %v", st.lastCreate["placement"])
	}
	if st.lastCreate["kind"] != "gateway" {
		t.Fatalf("kind = %v", st.lastCreate["kind"])
	}
}

func TestPairRefusesAPairedGateway(t *testing.T) {
	st, coord := newCoordStub(t)
	s := technocoreServer(t)
	seedIdentity(t, s.DataDir, coord, func(id *tc.Identity) { id.Status = tc.StatusActive })
	res := doJSON(t, s.ControlHandler(), "POST", "/api/v1/technocore/pair", map[string]any{"serverUrl": coord})
	if res.Code != http.StatusConflict {
		t.Fatalf("pair = %d %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), "piso gateway launch") {
		t.Fatalf("no CLI fallback in %s", res.Body)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.creates != 0 {
		t.Fatalf("created %d requests for a paired gateway", st.creates)
	}
}

// The URL comes from a form, so it must not be usable as an SSRF primitive.
func TestPairRefusesMetadataAndBadURLs(t *testing.T) {
	_, coord := newCoordStub(t)
	s := technocoreServer(t)
	h := s.ControlHandler()
	cases := map[string]string{
		"metadata IP":    "http://169.254.169.254",
		"metadata name":  "http://metadata.google.internal",
		"credentials":    "https://user:pw@example.com",
		"with path":      "https://piso.dev/app",
		"with query":     "https://piso.dev/?x=1",
		"empty":          "",
		"unsupported":    "ftp://piso.dev",
		"unspecified IP": "http://0.0.0.0:8000",
	}
	for name, raw := range cases {
		res := doJSON(t, h, "POST", "/api/v1/technocore/pair", map[string]any{"serverUrl": raw})
		if res.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, res.Code, res.Body)
		}
	}
	// Sanity: the stubbed coordinator is accepted, so the table above fails on
	// the guard rather than on every input.
	if res := doJSON(t, h, "POST", "/api/v1/technocore/pair", map[string]any{"serverUrl": coord}); res.Code != 200 {
		t.Fatalf("valid coordinator rejected: %d %s", res.Code, res.Body)
	}
}

func TestPairRequiresAHealthAnswer(t *testing.T) {
	st, coord := newCoordStub(t)
	st.healthStatus = http.StatusInternalServerError
	s := technocoreServer(t)
	res := doJSON(t, s.ControlHandler(), "POST", "/api/v1/technocore/pair", map[string]any{"serverUrl": coord})
	if res.Code != http.StatusBadGateway {
		t.Fatalf("pair = %d %s", res.Code, res.Body)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.creates != 0 {
		t.Fatal("minted a request against a coordinator that failed its health check")
	}
	if id := loadTestIdentity(t, s.DataDir); id.PairingID != "" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestConfirmNeedsThePhoneFirst(t *testing.T) {
	_, coord := newCoordStub(t)
	s := technocoreServer(t)
	h := s.ControlHandler()
	doJSON(t, h, "POST", "/api/v1/technocore/pair", map[string]any{"serverUrl": coord})
	res := doJSON(t, h, "POST", "/api/v1/technocore/pair/confirm", nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("confirm = %d %s", res.Code, res.Body)
	}
	if id := loadTestIdentity(t, s.DataDir); id.Status == tc.StatusActive {
		t.Fatal("confirmed without a phone approval")
	}
}

func TestConfirmEnrollsImmediately(t *testing.T) {
	st, coord := newCoordStub(t)
	s := technocoreServer(t)
	h := s.ControlHandler()
	doJSON(t, h, "POST", "/api/v1/technocore/pair", map[string]any{"serverUrl": coord})
	// The sync loop observes the phone's approval; the dashboard then confirms.
	st.mu.Lock()
	st.status = tc.PairStatusAwaitingGateway
	st.mu.Unlock()
	id := loadTestIdentity(t, s.DataDir)
	id.PairPhase = tc.PairStatusAwaitingGateway
	id.PairApprover = "alice"
	if err := tc.Save(tc.Path(s.DataDir), id); err != nil {
		t.Fatal(err)
	}

	res := doJSON(t, h, "POST", "/api/v1/technocore/pair/confirm", nil)
	if res.Code != 200 {
		t.Fatalf("confirm = %d %s", res.Code, res.Body)
	}
	body := decodeBody(t, res)
	if body["enrolled"] != true || body["status"] != tc.StatusActive {
		t.Fatalf("snapshot = %v", body)
	}
	if body["principalId"] != "principal-1" {
		t.Fatalf("principalId = %v", body["principalId"])
	}
	if body["pairUrl"] != nil || body["pairPhase"] != nil || body["awaitingConfirm"] != false {
		t.Fatalf("stale pair fields: %v", body)
	}
	// The identity is written before the response, so a restart right after the
	// tap does not lose the enrollment.
	if id := loadTestIdentity(t, s.DataDir); id.Status != tc.StatusActive || id.PrincipalID != "principal-1" {
		t.Fatalf("identity = %+v", id)
	}

	// A double click is a no-op, not a second principal.
	again := doJSON(t, h, "POST", "/api/v1/technocore/pair/confirm", nil)
	if again.Code != 200 || decodeBody(t, again)["enrolled"] != true {
		t.Fatalf("second confirm = %d %s", again.Code, again.Body)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.confirmCalled != 1 {
		t.Fatalf("confirm calls = %d, want 1", st.confirmCalled)
	}
}

// An approval with no signed-in person behind it mints nothing on purpose. The
// dashboard must hand the operator a signed-in URL instead of pretending it
// failed.
func TestConfirmReportsSignedInConfirmationNeeded(t *testing.T) {
	st, coord := newCoordStub(t)
	st.confirmLogin = true
	s := technocoreServer(t)
	h := s.ControlHandler()
	doJSON(t, h, "POST", "/api/v1/technocore/pair", map[string]any{"serverUrl": coord})
	id := loadTestIdentity(t, s.DataDir)
	id.PairPhase = tc.PairStatusAwaitingGateway
	if err := tc.Save(tc.Path(s.DataDir), id); err != nil {
		t.Fatal(err)
	}
	res := doJSON(t, h, "POST", "/api/v1/technocore/pair/confirm", nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("confirm = %d %s", res.Code, res.Body)
	}
	body := decodeBody(t, res)
	if body["needsLogin"] != true {
		t.Fatalf("needsLogin = %v", body["needsLogin"])
	}
	if body["confirmUrl"] != coord+"/pair/pair-1/" {
		t.Fatalf("confirmUrl = %v", body["confirmUrl"])
	}
	if id := loadTestIdentity(t, s.DataDir); id.Status == tc.StatusActive {
		t.Fatal("enrolled without a signed-in confirmation")
	}
}

func TestDeclineClearsTheAttempt(t *testing.T) {
	st, coord := newCoordStub(t)
	s := technocoreServer(t)
	h := s.ControlHandler()
	doJSON(t, h, "POST", "/api/v1/technocore/pair", map[string]any{"serverUrl": coord})
	id := loadTestIdentity(t, s.DataDir)
	id.PairPhase = tc.PairStatusAwaitingGateway
	id.PairApprover = "alice"
	if err := tc.Save(tc.Path(s.DataDir), id); err != nil {
		t.Fatal(err)
	}
	res := doJSON(t, h, "POST", "/api/v1/technocore/pair/decline", nil)
	if res.Code != 200 {
		t.Fatalf("decline = %d %s", res.Code, res.Body)
	}
	got := loadTestIdentity(t, s.DataDir)
	if got.Status != tc.StatusNew || got.PairingID != "" || got.PairPhase != "" {
		t.Fatalf("identity = %+v", got)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.declineCalled != 1 {
		t.Fatalf("declines = %d", st.declineCalled)
	}
}

func TestCancelIsLocalOnly(t *testing.T) {
	st, coord := newCoordStub(t)
	s := technocoreServer(t)
	h := s.ControlHandler()
	doJSON(t, h, "POST", "/api/v1/technocore/pair", map[string]any{"serverUrl": coord})
	res := doJSON(t, h, "POST", "/api/v1/technocore/pair/cancel", nil)
	if res.Code != 200 {
		t.Fatalf("cancel = %d %s", res.Code, res.Body)
	}
	if id := loadTestIdentity(t, s.DataDir); id.PairingID != "" || id.Status != tc.StatusNew {
		t.Fatalf("identity = %+v", id)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.declineCalled != 0 || st.confirmCalled != 0 {
		t.Fatal("cancel told the coordinator to decline or confirm")
	}
}

func TestCancelRefusesAPairedGateway(t *testing.T) {
	_, coord := newCoordStub(t)
	s := technocoreServer(t)
	seedIdentity(t, s.DataDir, coord, func(id *tc.Identity) {
		id.Status = tc.StatusActive
		id.PairingID = "pair-1"
	})
	res := doJSON(t, s.ControlHandler(), "POST", "/api/v1/technocore/pair/cancel", nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("cancel = %d %s", res.Code, res.Body)
	}
}

func TestQRCodeIsServedWhilePairing(t *testing.T) {
	_, coord := newCoordStub(t)
	s := technocoreServer(t)
	h := s.ControlHandler()

	if res := doJSON(t, h, "GET", "/api/v1/technocore/pair/qr.svg", nil); res.Code != 404 {
		t.Fatalf("qr before pairing = %d", res.Code)
	}
	doJSON(t, h, "POST", "/api/v1/technocore/pair", map[string]any{"serverUrl": coord})
	res := doJSON(t, h, "GET", "/api/v1/technocore/pair/qr.svg", nil)
	if res.Code != 200 {
		t.Fatalf("qr = %d %s", res.Code, res.Body)
	}
	if ct := res.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("content type = %q", ct)
	}
	if cc := res.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q", cc)
	}
	if !strings.HasPrefix(res.Body.String(), "<svg") || !strings.Contains(res.Body.String(), "<path") {
		t.Fatalf("not an svg: %.80q", res.Body.String())
	}
}

// The SVG must carry the real bitmap, not a decorative pattern: every dark
// module becomes one 1x1 path square. Counting against the library's own
// bitmap checks the renderer, which is the part written here.
func TestQRSVGEncodesTheBitmap(t *testing.T) {
	url := "https://piso.dev/pair/11111111-2222-3333-4444-555555555555/"
	svg, err := qrSVG(url)
	if err != nil {
		t.Fatal(err)
	}
	other, err := qrSVG("https://piso.dev/pair/99999999-2222-3333-4444-555555555555/")
	if err != nil {
		t.Fatal(err)
	}
	if string(svg) == string(other) {
		t.Fatal("two different urls produced the same code")
	}
	if strings.Contains(string(svg), url) {
		t.Fatal("the url was embedded as text instead of encoded")
	}

	q, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		t.Fatal(err)
	}
	bmp := q.Bitmap()
	dark := 0
	for _, row := range bmp {
		for _, on := range row {
			if on {
				dark++
			}
		}
	}
	if got := strings.Count(string(svg), "M"); got != dark {
		t.Fatalf("path squares = %d, dark modules = %d", got, dark)
	}
	// Bitmap() already includes the four-module quiet zone; the viewBox must
	// match it or the code clips.
	if !strings.Contains(string(svg), fmt.Sprintf("viewBox=\"0 0 %d %d\"", len(bmp), len(bmp))) {
		t.Fatalf("viewBox does not match a %d-module bitmap", len(bmp))
	}
	if _, err := qrSVG(""); err == nil {
		t.Fatal("encoded an empty payload")
	}
}

func TestStatusCarriesTheApproverAndVaultCounts(t *testing.T) {
	_, coord := newCoordStub(t)
	s := technocoreServer(t)
	seedIdentity(t, s.DataDir, coord, func(id *tc.Identity) {
		id.Status = tc.StatusPending
		id.PairingID = "pair-1"
		id.PairPhase = tc.PairStatusAwaitingGateway
		id.PairApprover = "alice"
		id.PairAccount = "Alice"
		id.PairExpiresAt = time.Now().Add(9 * time.Minute).UTC().Format(time.RFC3339)
	})
	if err := s.Store.AddSecret(store.SecretRec{
		ID: "tc_one", Name: "OPENAI", Placeholder: "piso_tc_one", Value: "sk-1",
		TechnocoreID: "one", PublishedHash: "h", ContentVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AddSecret(store.SecretRec{
		ID: "manual", Name: "LOCAL", Placeholder: "piso_manual", Value: "v",
	}); err != nil {
		t.Fatal(err)
	}

	res := doJSON(t, s.ControlHandler(), "GET", "/api/v1/technocore", nil)
	body := decodeBody(t, res)
	if body["awaitingConfirm"] != true || body["enrolled"] != false {
		t.Fatalf("snapshot = %v", body)
	}
	if body["pairApprover"] != "alice" || body["pairAccount"] != "Alice" {
		t.Fatalf("approver = %v %v", body["pairApprover"], body["pairAccount"])
	}
	if body["status"] != "awaiting_gateway" {
		t.Fatalf("status = %v", body["status"])
	}
	counts, _ := body["secrets"].(map[string]any)
	if counts["total"] != float64(2) || counts["inbound"] != float64(1) || counts["published"] != float64(1) {
		t.Fatalf("counts = %v", counts)
	}
}

func TestNormalizeCoordinatorURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"piso.dev", "https://piso.dev"},
		{"https://piso.dev/", "https://piso.dev"},
		{"  https://piso.dev  ", "https://piso.dev"},
		{"http://127.0.0.1:8000", "http://127.0.0.1:8000"},
		{"https://api.armortron9000.com", "https://api.armortron9000.com"},
	}
	for _, c := range cases {
		got, err := normalizeCoordinatorURL(c.in)
		if err != nil || got != c.want {
			t.Fatalf("normalize(%q) = %q, %v", c.in, got, err)
		}
	}
}
