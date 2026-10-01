package technocore

import (
	"testing"
	"time"
)

func TestGrowBackoff(t *testing.T) {
	if got := growBackoff(0); got != 2*time.Second {
		t.Fatalf("from 0: got %v", got)
	}
	if got := growBackoff(time.Second); got != 2*time.Second {
		t.Fatalf("from 1s: got %v", got)
	}
	if got := growBackoff(time.Minute); got != 2*time.Minute {
		t.Fatalf("from 1m: got %v", got)
	}
	if got := growBackoff(2 * time.Minute); got != 2*time.Minute {
		t.Fatalf("cap: got %v", got)
	}
}

func TestJitterDurationRange(t *testing.T) {
	if got := jitterDuration(0); got != 0 {
		t.Fatalf("zero: got %v", got)
	}
	d := 10 * time.Second
	lo, hi := d-2*time.Second, d+2*time.Second
	for i := 0; i < 50; i++ {
		got := jitterDuration(d)
		if got < lo || got > hi {
			t.Fatalf("jitter %v outside [%v, %v]", got, lo, hi)
		}
	}
}

func TestNudgeDoesNotBlock(t *testing.T) {
	r := newReconciler(nil, "")
	done := make(chan struct{})
	go func() {
		r.Nudge()
		r.Nudge()
		r.Nudge()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Nudge blocked")
	}
	select {
	case <-r.nudge:
	default:
		t.Fatal("expected one coalesced nudge")
	}
	select {
	case <-r.nudge:
		t.Fatal("nudge channel should stay at most one")
	default:
	}
}
