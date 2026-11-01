package server

import (
	"encoding/json"
	"testing"
	"time"

	"piso/gateway/internal/store"
)

func TestSupervisorReadinessRequiresMailboxPoll(t *testing.T) {
	s := testServer(t)
	seedSupervisorTarget(t, s, "prin-ship")
	setLiveSupervisor(t, s, mailWorker)
	status := func() *supervisorStatus {
		t.Helper()
		w := doJSON(t, s.ControlHandler(), "GET", "/api/v1/telegram", nil)
		var out telegramStatus
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Supervisor
	}
	if got := status(); got == nil || got.Ready || got.LastMailboxPoll != nil {
		t.Fatalf("lease alone: %+v", got)
	}
	if s.Store.MarkSupervisorMailboxPoll("piso-worker-other", time.Now()) {
		t.Fatal("non-holder marked ready")
	}
	// A completed empty long-poll is sufficient; no chat message is needed.
	w := doJSON(t, s.WorkerHandler(), "GET", "/api/v1/worker/messages?worker="+mailWorker+"&wait=1", nil)
	if w.Code != 200 {
		t.Fatalf("poll: %d %s", w.Code, w.Body.String())
	}
	if got := status(); got == nil || !got.Ready || got.LastMailboxPoll == nil {
		t.Fatalf("successful poll: %+v", got)
	}
	link := s.Store.SupervisorLink()
	if err := s.Store.SetSupervisorLink(link); err != nil {
		t.Fatal(err)
	}
	if got := status(); got == nil || !got.Ready {
		t.Fatalf("renewal lost readiness: %+v", got)
	}
	s.Store.MarkSupervisorMailboxPoll(mailWorker, time.Now().Add(-61*time.Second))
	if got := status(); got == nil || got.Ready {
		t.Fatalf("stale poll: %+v", got)
	}
	link.ExpiresAt = time.Now().Add(-time.Second)
	if err := s.Store.SetSupervisorLink(link); err != nil {
		t.Fatal(err)
	}
	if s.Store.MarkSupervisorMailboxPoll(mailWorker, time.Now()) {
		t.Fatal("expired holder marked ready")
	}
	if got := status(); got != nil {
		t.Fatalf("expired lease: %+v", got)
	}
	// A handoff must never inherit the previous worker's readiness.
	if err := s.Store.SetSupervisorLink(store.SupervisorLink{Worker: "piso-worker-next", PrincipalID: "next", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := status(); got == nil || got.Ready || got.LastMailboxPoll != nil {
		t.Fatalf("handoff: %+v", got)
	}
}
