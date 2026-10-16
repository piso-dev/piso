package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"piso/gateway/internal/model"
	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

// enrolledServer points the gateway's coordinator client at an in-process
// coordinator. The identity is written to the server's data directory so
// technocore.Call has somewhere to sign and dial.
func enrolledServer(t *testing.T, coord http.HandlerFunc) *Server {
	t.Helper()
	srv := httptest.NewServer(coord)
	t.Cleanup(srv.Close)
	s := testServer(t)
	dir := t.TempDir()
	s.DataDir = dir
	id, err := tc.Generate(srv.URL, "gw")
	if err != nil {
		t.Fatal(err)
	}
	id.Status = tc.StatusActive
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}
	return s
}

// seedSupervisorTarget stores a published bot and one standalone worker with a
// coordinator principal, so the handler has something to nominate.
func seedSupervisorTarget(t *testing.T, s *Server, principalID string) {
	t.Helper()
	if err := s.Store.AddSecret(store.SecretRec{
		ID: "sec_bot", Name: "Telegram bot", Kind: model.SecretKindTelegram,
		GatewayOnly: true, Value: botToken,
		TechnocoreID: "11111111-1111-1111-1111-111111111111",
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.UpsertWorker(store.WorkerRec{
		Name: "piso-worker-ship", Slug: "ship", PrincipalID: principalID,
		IPs: []string{"192.0.2.1"},
	}); err != nil {
		t.Fatal(err)
	}
}

func supervisorAsk(t *testing.T, s *Server, worker string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, s.WorkerHandler(), "POST", "/api/v1/worker/supervisor", map[string]any{"worker": worker})
}

func TestWorkerSupervisorGrantIsCached(t *testing.T) {
	var gotWorker string
	s := enrolledServer(t, func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if id, _ := in["worker_id"].(string); id != "" {
			gotWorker = id
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"granted":true,"holder":"w1","holder_principal":"prin-ship","self":true,"expires_at":"2099-01-02T03:04:05+00:00"}`))
	})
	seedSupervisorTarget(t, s, "prin-ship")

	w := supervisorAsk(t, s, "piso-worker-ship")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"granted":true`) {
		t.Fatalf("grant %d %s", w.Code, w.Body.String())
	}
	if gotWorker != "prin-ship" {
		t.Fatalf("coordinator saw worker_id %q", gotWorker)
	}
	if link, live := s.Store.Supervisor(time.Now()); !live || link.Worker != "piso-worker-ship" {
		t.Fatalf("supervisor not cached live: %+v", link)
	}
	// The dashboard reads the same cache, so it must show the supervisor.
	status := doJSON(t, s.ControlHandler(), "GET", "/api/v1/telegram", nil).Body.String()
	if !strings.Contains(status, `"supervisor":{"worker":"piso-worker-ship"`) {
		t.Fatalf("status missing supervisor: %s", status)
	}
}

func TestWorkerSupervisorRefusalIsNotAFailure(t *testing.T) {
	s := enrolledServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"granted":false,"reason":"supervisor_taken"}`))
	})
	seedSupervisorTarget(t, s, "prin-ship")

	w := supervisorAsk(t, s, "piso-worker-ship")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reason":"supervisor_taken"`) {
		t.Fatalf("refusal %d %s", w.Code, w.Body.String())
	}
	if _, live := s.Store.Supervisor(time.Now()); live {
		t.Fatal("a refused worker must not be cached as live")
	}
}

// A worker that was the supervisor and then lost the lease must stop being
// reported as live, so the delivery path does not route to it.
func TestWorkerSupervisorLossClearsTheCache(t *testing.T) {
	granted := true
	s := enrolledServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if granted {
			_, _ = w.Write([]byte(`{"granted":true,"holder_principal":"prin-ship","expires_at":"2099-01-02T03:04:05+00:00"}`))
			return
		}
		_, _ = w.Write([]byte(`{"granted":false,"reason":"supervisor_taken"}`))
	})
	seedSupervisorTarget(t, s, "prin-ship")
	if w := supervisorAsk(t, s, "piso-worker-ship"); w.Code != 200 {
		t.Fatalf("grant %d %s", w.Code, w.Body.String())
	}
	granted = false
	if w := supervisorAsk(t, s, "piso-worker-ship"); w.Code != 200 {
		t.Fatalf("refusal %d %s", w.Code, w.Body.String())
	}
	if _, live := s.Store.Supervisor(time.Now()); live {
		t.Fatal("a lost lease must not stay live in the cache")
	}
	if link := s.Store.SupervisorLink(); link.Reason != "supervisor_taken" {
		t.Fatalf("loss reason not recorded: %+v", link)
	}
}

func TestWorkerSupervisorNeedsAPublishedBot(t *testing.T) {
	s := enrolledServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the coordinator must not be called without a published bot")
	})
	if _, err := s.Store.UpsertWorker(store.WorkerRec{
		Name: "piso-worker-ship", Slug: "ship", PrincipalID: "prin-ship",
		IPs: []string{"192.0.2.1"},
	}); err != nil {
		t.Fatal(err)
	}
	if w := supervisorAsk(t, s, "piso-worker-ship"); w.Code != 409 {
		t.Fatalf("want 409, got %d %s", w.Code, w.Body.String())
	}
}

func TestWorkerSupervisorNeedsACoordinatorIdentity(t *testing.T) {
	s := enrolledServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the coordinator must not be called without a worker identity")
	})
	seedSupervisorTarget(t, s, "")
	if w := supervisorAsk(t, s, "piso-worker-ship"); w.Code != 409 {
		t.Fatalf("want 409, got %d %s", w.Code, w.Body.String())
	}
}

func TestWorkerSupervisorRejectsAClaimedIdentity(t *testing.T) {
	s := enrolledServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("a misattributed ask must not reach the coordinator")
	})
	seedSupervisorTarget(t, s, "prin-ship")
	if _, err := s.Store.UpsertWorker(store.WorkerRec{
		Name: "piso-worker-other", Slug: "other", PrincipalID: "prin-other",
		IPs: []string{"192.0.2.1"},
	}); err != nil {
		t.Fatal(err)
	}
	if w := supervisorAsk(t, s, "piso-worker-ship"); w.Code != 403 {
		t.Fatalf("want 403, got %d %s", w.Code, w.Body.String())
	}
}

func TestWorkerSupervisorReportsACoordinatorFailure(t *testing.T) {
	s := enrolledServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	seedSupervisorTarget(t, s, "prin-ship")
	if w := supervisorAsk(t, s, "piso-worker-ship"); w.Code != 502 {
		t.Fatalf("want 502, got %d %s", w.Code, w.Body.String())
	}
}
