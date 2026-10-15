package store

import (
	"testing"
	"time"
)

func TestMailboxLifecycle(t *testing.T) {
	st := testStore(t)
	now := time.Now()

	enq, err := st.EnqueueMail(MailMessage{ChatID: 9, UserID: 9, Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if enq.ID == "" || enq.State != MailPending {
		t.Fatalf("enqueued %+v", enq)
	}
	if !st.PendingMail() {
		t.Fatal("a pending message must report as pending")
	}

	taken := st.TakeMail(now, 20)
	if len(taken) != 1 || taken[0].ID != enq.ID || taken[0].State != MailDelivered {
		t.Fatalf("taken %+v", taken)
	}
	// Handing out is not removal: without an ack the message is still there.
	if len(st.Mail()) != 1 {
		t.Fatalf("mailbox %+v", st.Mail())
	}
	// A second take while it is delivered returns nothing.
	if again := st.TakeMail(now, 20); len(again) != 0 {
		t.Fatalf("redelivered too early: %+v", again)
	}

	if err := st.AckMail([]string{enq.ID}); err != nil {
		t.Fatal(err)
	}
	if len(st.Mail()) != 0 || st.PendingMail() {
		t.Fatalf("mailbox not empty: %+v", st.Mail())
	}
	// Acking an unknown id is harmless (a redelivered message acked twice).
	if err := st.AckMail([]string{"nope"}); err != nil {
		t.Fatal(err)
	}
}

func TestMailboxRedeliversAfterVisibility(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	if _, err := st.EnqueueMail(MailMessage{ChatID: 1, Text: "one"}); err != nil {
		t.Fatal(err)
	}
	if got := st.TakeMail(now, 20); len(got) != 1 {
		t.Fatalf("first take %+v", got)
	}
	// The worker died mid-turn: no ack before the window closes.
	late := st.TakeMail(now.Add(MailVisibility), 20)
	if len(late) != 1 {
		t.Fatalf("expected a redelivery, got %+v", late)
	}
	if late[0].Attempts != 1 {
		t.Fatalf("attempts %d", late[0].Attempts)
	}
}

func TestMailboxIsBounded(t *testing.T) {
	st := testStore(t)
	for i := 0; i < mailboxCap+5; i++ {
		if _, err := st.EnqueueMail(MailMessage{ChatID: int64(i), Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	mail := st.Mail()
	if len(mail) != mailboxCap {
		t.Fatalf("len %d", len(mail))
	}
	// The oldest were dropped, so the newest survives.
	if mail[len(mail)-1].ChatID != int64(mailboxCap+4) {
		t.Fatalf("newest lost: %+v", mail[len(mail)-1])
	}
}

func TestTakeMailRequeuesOnlyExpiredDeliveries(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	if _, err := st.EnqueueMail(MailMessage{ChatID: 1, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueMail(MailMessage{ChatID: 2, Text: "b"}); err != nil {
		t.Fatal(err)
	}
	first := st.TakeMail(now, 1) // takes only "a"
	if len(first) != 1 || first[0].ChatID != 1 {
		t.Fatalf("first %+v", first)
	}
	// "b" is still pending and is taken on the next poll.
	second := st.TakeMail(now, 20)
	if len(second) != 1 || second[0].ChatID != 2 {
		t.Fatalf("second %+v", second)
	}
	// Nothing is due yet.
	if third := st.TakeMail(now, 20); len(third) != 0 {
		t.Fatalf("third %+v", third)
	}
}
