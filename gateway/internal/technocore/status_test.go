package technocore

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tc "piso/internal/technocore"
)

// liveStatus is process-wide; every test in this file starts and ends clean so
// an earlier sync cannot leak a timestamp into a later snapshot.
func resetLiveStatus(t *testing.T) {
	t.Helper()
	setLive(func(s *Status) { *s = Status{} })
	t.Cleanup(func() { setLive(func(s *Status) { *s = Status{} }) })
}

func writeIdentity(t *testing.T, dir, status, pairingID string) {
	t.Helper()
	id, err := tc.Generate("https://tc.example", "gateway")
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	id.Status = status
	id.PairingID = pairingID
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatalf("save identity: %v", err)
	}
}

// An unpaired gateway has never synced. Reporting a zero time rendered as
// "synced 739905d ago" in the dashboard.
func TestSnapshotUnpairedOmitsSyncAge(t *testing.T) {
	resetLiveStatus(t)
	got := Snapshot(t.TempDir())
	if got.Status != "unpaired" {
		t.Fatalf("status = %q, want unpaired", got.Status)
	}
	if got.Enrolled {
		t.Fatal("unpaired snapshot claims enrollment")
	}
	if got.LastSync != nil {
		t.Fatalf("unpaired snapshot reports lastSync %s", got.LastSync)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "lastSync") {
		t.Fatalf("unpaired snapshot serializes a sync time: %s", raw)
	}
}

func TestSnapshotPendingDropsStaleSyncAge(t *testing.T) {
	resetLiveStatus(t)
	stale := time.Now().UTC().Add(-time.Hour)
	setLive(func(s *Status) { s.LastSync = &stale })
	dir := t.TempDir()
	writeIdentity(t, dir, tc.StatusPending, "pair-1")

	got := Snapshot(dir)
	if got.Status != tc.StatusPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}
	if got.PairURL != "https://tc.example/pair/pair-1/" {
		t.Fatalf("pairUrl = %q", got.PairURL)
	}
	if got.LastSync != nil {
		t.Fatalf("pending snapshot reports lastSync %s", got.LastSync)
	}
}

func TestSnapshotActiveReportsLiveSync(t *testing.T) {
	resetLiveStatus(t)
	dir := t.TempDir()
	writeIdentity(t, dir, tc.StatusActive, "")

	if got := Snapshot(dir); got.LastSync != nil {
		t.Fatalf("active snapshot before any sync reports lastSync %s", got.LastSync)
	}

	synced := time.Now().UTC().Truncate(time.Second)
	setLive(func(s *Status) { s.LastSync = &synced })
	got := Snapshot(dir)
	if !got.Enrolled {
		t.Fatal("active identity not reported as enrolled")
	}
	if got.LastSync == nil || !got.LastSync.Equal(synced) {
		t.Fatalf("lastSync = %v, want %v", got.LastSync, synced)
	}
}
