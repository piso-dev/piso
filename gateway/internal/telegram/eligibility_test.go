package telegram

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestManagerWaitsForEligibleWorkerWithoutPolling(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	m.Lease = func(context.Context, string, int64) (LeaseGrant, error) {
		return LeaseGrant{Reason: "no_eligible_worker"}, nil
	}
	run(t, m)
	waitFor(t, "eligibility refusal", func() bool { return m.Snapshot().Waiting })
	snap := m.Snapshot()
	if snap.WaitReason != "no_eligible_worker" || snap.Running || snap.LeaseHeld || snap.LastError != "" {
		t.Fatalf("unexpected status: %+v", snap)
	}
	if len(fb.askedOffsets()) != 0 {
		t.Fatal("an ineligible gateway polled Telegram")
	}
}

func TestManagerStopsWhenWorkerEligibilityRenewalIsRefused(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	advance := fakeClock(m)
	m.Lease = func(context.Context, string, int64) (LeaseGrant, error) {
		return LeaseGrant{Reason: "no_eligible_worker"}, nil
	}
	rec, _ := m.bot()
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- m.session(ctx, rec, LeaseGrant{Granted: true, ExpiresAt: time.Now().Add(90 * time.Second)})
	}()
	waitFor(t, "first poll", func() bool { return len(fb.askedOffsets()) == 1 })
	advance(leaseRenew)
	fb.push()
	select {
	case err := <-done:
		if !errors.Is(err, errLeaseLost) {
			t.Fatalf("session error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("session kept polling after refusal")
	}
	if len(fb.askedOffsets()) != 1 || m.Snapshot().WaitReason != "no_eligible_worker" {
		t.Fatalf("polls=%v status=%+v", fb.askedOffsets(), m.Snapshot())
	}
}

func TestManagerBoundsInflightPollByCandidateExpiry(t *testing.T) {
	st := newStore(t)
	addPublishedToken(t, st, goodToken)
	fb := newFakeBot(42, "piso_bot")
	m := NewManager(st, fb.client())
	m.Lease = func(context.Context, string, int64) (LeaseGrant, error) {
		t.Error("should not renew before the normal renewal interval")
		return LeaseGrant{}, nil
	}
	rec, _ := m.bot()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	err := m.session(ctx, rec, LeaseGrant{Granted: true, ExpiresAt: start.Add(50 * time.Millisecond)})
	if !errors.Is(err, errLeaseLost) {
		t.Fatalf("want lease expiry, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("poll outlived candidate expiry: %v", elapsed)
	}
	if len(fb.askedOffsets()) != 1 {
		t.Fatalf("unexpected polls: %v", fb.askedOffsets())
	}
}
