package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestTelegramAssignmentsWithoutLocalBotOrWorker(t *testing.T) {
	s := enrolledServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/telegram/assignments" {
			t.Errorf("unexpected coordinator request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"bots":[{"resourceId":"bot1","name":"Telegram bot","supervisor":{"workerId":"w1","principalId":"p1","name":"Ship","gatewayId":"g1","gatewayName":"Virginia","expiresAt":"2099-01-02T03:04:05Z"}}]}`))
	})
	out := doJSON(t, s.ControlHandler(), "GET", "/api/v1/telegram/assignments", nil)
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"gatewayName":"Virginia"`) {
		t.Fatalf("shared assignments: %d %s", out.Code, out.Body.String())
	}
	if s.hasLiveSupervisor() {
		t.Fatal("remote assignment must not authorize local mailbox delivery")
	}
	if len(s.Store.Workers()) != 0 || len(s.Store.Secrets()) != 0 {
		t.Fatal("reading shared state must not create local workers or credentials")
	}
}

func TestTelegramAssignmentsUnavailableIsNotAnEmptyAccount(t *testing.T) {
	s := enrolledServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	out := doJSON(t, s.ControlHandler(), "GET", "/api/v1/telegram/assignments", nil)
	if out.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d %s", out.Code, out.Body.String())
	}
}

func TestTelegramAssignmentsForwardsCheckingState(t *testing.T) {
	s := enrolledServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"bots":[{"resourceId":"bot1","state":"checking","checkingUntil":"2099-01-02T03:04:05Z","supervisor":null,"supervisorState":"checking","supervisorCheckingUntil":"2099-01-02T03:04:05Z"}]}`))
	})
	out := doJSON(t, s.ControlHandler(), "GET", "/api/v1/telegram/assignments", nil)
	for _, field := range []string{`"state":"checking"`, `"supervisorState":"checking"`, `"supervisor":null`, `"checkingUntil":"2099-01-02T03:04:05Z"`, `"supervisorCheckingUntil":"2099-01-02T03:04:05Z"`} {
		if out.Code != 200 || !strings.Contains(out.Body.String(), field) {
			t.Fatalf("missing forwarded field %s: %d %s", field, out.Code, out.Body.String())
		}
	}
}

func TestTelegramAssignmentsEmptyAccount(t *testing.T) {
	s := enrolledServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"bots":[]}`))
	})
	out := doJSON(t, s.ControlHandler(), "GET", "/api/v1/telegram/assignments", nil)
	if out.Code != 200 || strings.TrimSpace(out.Body.String()) != `{"bots":[]}` {
		t.Fatalf("empty assignments: %d %s", out.Code, out.Body.String())
	}
}
