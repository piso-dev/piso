package server

import (
	"encoding/json"
	"testing"
	"time"

	"piso/gateway/internal/store"
)

func TestTimelineThreeHourWindow(t *testing.T) {
	s := testServer(t)
	now := time.Now().UnixMilli()
	for _, a := range []store.Activity{
		{Slug: "older", Worker: "worker-older", Kind: "note", Text: "outside window", Ts: now - 7*3600000},
		{Slug: "first", Worker: "worker-first", Kind: "note", Text: "two hours ago", Ts: now - 2*3600000},
		{Slug: "second", Worker: "worker-second", Kind: "progress", Text: "recent", Ts: now - 1000},
	} {
		if _, err := s.Store.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	// The window overrides a row cap, retaining every project in the window.
	res := doJSON(t, s.ControlHandler(), "GET", "/api/v1/activities?window=3h&limit=1", nil)
	if res.Code != 200 {
		t.Fatalf("window status %d: %s", res.Code, res.Body.String())
	}
	var rows []store.Activity
	if err := json.Unmarshal(res.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Slug != "second" || rows[1].Slug != "first" {
		t.Fatalf("window rows: %+v", rows)
	}
	// Existing limit-based callers keep their previous behavior.
	res = doJSON(t, s.ControlHandler(), "GET", "/api/v1/activities?limit=1", nil)
	if err := json.Unmarshal(res.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Slug != "second" {
		t.Fatalf("limited rows: %+v", rows)
	}
}
