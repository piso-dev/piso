package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"piso/gateway/internal/store"
)

func TestWorkerPostPortsCreatesAutoRoutes(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/worker/ports", bytes.NewReader([]byte(
		`{"worker":"piso-worker-demo","slug":"demo","ports":[{"port":8080,"reachable":true},{"port":14970,"reachable":false,"note":"bound to loopback 127.0.0.1"}]}`,
	)))
	req.Header.Set("Content-Type", "application/json")
	wh.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("post ports %d %s", w.Code, w.Body.String())
	}
	r, ok := s.Store.RouteByName("demo-8080")
	if !ok || r.Worker != "piso-worker-demo" || r.Port != 8080 || r.Origin != "auto" {
		t.Fatalf("route %+v ok=%v", r, ok)
	}
	// unreachable hint recorded
	hints, ok := s.Store.WorkerUnreachable("piso-worker-demo")
	if !ok || len(hints) != 1 || hints[0].Port != 14970 {
		t.Fatalf("hints %+v ok=%v", hints, ok)
	}
	// origin + heartbeat serialize (dashboard depends on them)
	rec, _ := s.Store.RouteByName("demo-8080")
	body, _ := json.Marshal(rec)
	if !strings.Contains(string(body), `"origin":"auto"`) || !strings.Contains(string(body), `"lastSeenMs"`) {
		t.Fatalf("route view missing fields: %s", body)
	}
}

func TestWorkerPostPortsSlugDefaultsAndDedupes(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/worker/ports", bytes.NewReader([]byte(
		`{"worker":"piso-worker-mixed","ports":[
		   {"port":8080,"reachable":true},{"port":8080,"reachable":true},
		   {"port":99999,"reachable":true},{"port":0,"reachable":true}]}`,
	)))
	req.Header.Set("Content-Type", "application/json")
	wh.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("post %d %s", w.Code, w.Body.String())
	}
	r, ok := s.Store.RouteByName("mixed-8080") // slug derived from worker name
	if !ok || r.Origin != "auto" {
		t.Fatalf("dedupe/slug route %+v ok=%v", r, ok)
	}
	// 99999 (invalid) and 0 were dropped, so no out-of-range route exists
	if _, found := s.Store.RouteByName("mixed-99999"); found {
		t.Fatal("invalid port was routed")
	}
	if _, found := s.Store.RouteByName("mixed-0"); found {
		t.Fatal("zero port was routed")
	}
}

func TestWorkerPostPortsValidation(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()

	if w := doJSON(t, wh, "POST", "/api/v1/worker/ports", map[string]any{
		"worker": "../etc", "ports": []map[string]any{{"port": 8080, "reachable": true}},
	}); w.Code != 400 {
		t.Fatalf("bad worker %d", w.Code)
	}
	w := httptest.NewRecorder()
	wh.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/worker/ports", strings.NewReader("{not json")))
	if w.Code != 400 {
		t.Fatalf("bad json %d %s", w.Code, w.Body.String())
	}
}

func TestWorkerPostPortsIdentityMismatch(t *testing.T) {
	s := testServer(t)
	// register a worker whose vpc IP is the request's source
	if w := doJSON(t, s.ControlHandler(), "POST", "/api/v1/workers", map[string]any{
		"name": "piso-worker-demo", "slug": "demo", "ips": []string{"192.168.107.50"},
	}); w.Code != 200 {
		t.Fatalf("register %d %s", w.Code, w.Body.String())
	}
	// same source IP, claiming a different identity → rejected
	raw, _ := json.Marshal(map[string]any{
		"worker": "piso-worker-other", "slug": "other", "ports": []map[string]any{{"port": 8080, "reachable": true}},
	})
	req := httptest.NewRequest("POST", "/api/v1/worker/ports", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.168.107.50:12345"
	w := httptest.NewRecorder()
	s.WorkerHandler().ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("identity spoof accepted: %d %s", w.Code, w.Body.String())
	}
}

// A disabled route refuses to proxy (403) but stays listed for the toggle;
// re-enabling serves again.
func TestRouteDisabledBlocksProxy(t *testing.T) {
	s := testServer(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("from-worker"))
	}))
	t.Cleanup(backend.Close)
	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.AddRoute(store.RouteRec{ID: "r1", Name: "demo-8080", Worker: "127.0.0.1", Port: port, Origin: store.AutoRouteOriginAuto, LastSeenMs: 1}); err != nil {
		t.Fatal(err)
	}
	h := s.WebHandler()
	hit := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://demo-8080.piso.local/", nil)
		req.Host = "demo-8080.piso.local"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := hit(); w.Code != 200 || !strings.Contains(w.Body.String(), "from-worker") {
		t.Fatalf("pre-disable %d %s", w.Code, w.Body.String())
	}
	// disable via the control API
	raw, _ := json.Marshal(map[string]any{"disabled": true})
	req := httptest.NewRequest("POST", "/api/v1/routes/r1/disabled", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	wc := httptest.NewRecorder()
	s.ControlHandler().ServeHTTP(wc, req)
	if wc.Code != 200 {
		t.Fatalf("disable api %d %s", wc.Code, wc.Body.String())
	}
	if w := hit(); w.Code != http.StatusForbidden {
		t.Fatalf("disabled route should 403, got %d %s", w.Code, w.Body.String())
	}
	// re-enable
	raw, _ = json.Marshal(map[string]any{"disabled": false})
	req = httptest.NewRequest("POST", "/api/v1/routes/r1/disabled", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	wc = httptest.NewRecorder()
	s.ControlHandler().ServeHTTP(wc, req)
	if wc.Code != 200 {
		t.Fatalf("enable api %d %s", wc.Code, wc.Body.String())
	}
	if w := hit(); w.Code != 200 {
		t.Fatalf("re-enabled should serve, got %d", w.Code)
	}
}

func TestWorkerActivityPostGetRevoke(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	// post an activity
	res := doJSON(t, wh, "POST", "/api/v1/worker/activity", map[string]any{
		"worker": "piso-worker-demo", "kind": "progress", "text": "Started OAuth flow",
	})
	if res.Code != 201 {
		t.Fatalf("post %d %s", res.Code, res.Body.String())
	}
	var act store.Activity
	_ = json.Unmarshal(res.Body.Bytes(), &act)
	if act.ID == "" || act.Kind != "progress" || act.Slug != "demo" || act.Text != "Started OAuth flow" {
		t.Fatalf("act %+v", act)
	}
	// control plane read (host board)
	ctrl := doJSON(t, s.ControlHandler(), "GET", "/api/v1/activities", nil)
	if ctrl.Code != 200 || !strings.Contains(ctrl.Body.String(), act.ID) {
		t.Fatalf("control read %d %s", ctrl.Code, ctrl.Body.String())
	}
	// worker feed read
	feed := doJSON(t, wh, "GET", "/api/v1/worker/activities?worker=piso-worker-demo", nil)
	if feed.Code != 200 || !strings.Contains(feed.Body.String(), "Started OAuth flow") {
		t.Fatalf("feed %d %s", feed.Code, feed.Body.String())
	}
	// revoke by a different worker → 404 (scoped)
	rev := doJSON(t, wh, "POST", "/api/v1/worker/activity/revoke", map[string]any{"id": act.ID, "worker": "piso-worker-other"})
	if rev.Code != 404 {
		t.Fatalf("cross-worker revoke %d %s", rev.Code, rev.Body.String())
	}
	// revoke by owner → 204
	rev = doJSON(t, wh, "POST", "/api/v1/worker/activity/revoke", map[string]any{"id": act.ID, "worker": "piso-worker-demo"})
	if rev.Code != 204 {
		t.Fatalf("own revoke %d %s", rev.Code, rev.Body.String())
	}
}

func TestWorkerActivityFeedSpanProjection(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	now := time.Now().UnixNano() / 1000000
	// 4 beats 30s apart (one run) … a note … then a lone beat 20h earlier
	for _, a := range []store.Activity{
		store.Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindActive, Text: "working on x", Ts: now - 0},
		store.Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindActive, Text: "working on x", Ts: now - 30000},
		store.Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindActive, Text: "working on x", Ts: now - 60000},
		store.Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindActive, Text: "working on x", Ts: now - 90000},
		store.Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindNote, Text: "keep me", Ts: now - 120000},
		store.Activity{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindActive, Text: "working on x", Ts: now - 20*3600*1000},
	} {
		if _, err := s.Store.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	// raw feed: all 6 rows survive (store keeps full history)
	raw := doJSON(t, wh, "GET", "/api/v1/worker/activities?worker=piso-worker-demo", nil)
	var rawActs []store.Activity
	_ = json.Unmarshal(raw.Body.Bytes(), &rawActs)
	if raw.Code != 200 || len(rawActs) != 6 {
		t.Fatalf("raw feed %d %d rows", raw.Code, len(rawActs))
	}
	// span projection: the 4-beat run fuses into one row; the 20h-later beat
	// stays separate (gap > 10 min); the note is untouched
	sp := doJSON(t, wh, "GET", "/api/v1/worker/activities?worker=piso-worker-demo&active=span", nil)
	var acts []store.Activity
	_ = json.Unmarshal(sp.Body.Bytes(), &acts)
	if sp.Code != 200 || len(acts) != 3 {
		t.Fatalf("span feed %d %d rows: %s", sp.Code, len(acts), sp.Body.String())
	}
	var note, lone, run int
	for _, a := range acts {
		if a.Kind == store.ActivityKindNote {
			note += 1
			if a.Text != "keep me" || a.ActiveBeats != 0 {
				t.Fatalf("note mutated: %+v", a)
			}
		}
		if a.Kind == store.ActivityKindActive {
			if a.ActiveBeats >= 2 {
				run += 1
				if a.ActiveBeats != 4 || a.ActiveFromMs != now-90000 || !strings.Contains(a.Text, "· for 1 min") {
					t.Fatalf("run row: %+v", a)
				}
			} else {
				lone += 1
			}
		}
	}
	if note != 1 || run != 1 || lone != 1 {
		t.Fatalf("note=%d run=%d lone=%d", note, run, lone)
	}
}

func TestWorkerActivityFeedOmitsSessionStartedKeepsEndedAndIdle(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	now := time.Now().UnixNano() / 1000000
	for _, a := range []store.Activity{
		{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindNote, Text: "session started", Ts: now - 90000},
		{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindProgress, Text: "real work", Ts: now - 60000},
		{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindIdle, Text: "idle at prompt", Ts: now - 30000},
		{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindNote, Text: "session ended", Ts: now},
	} {
		if _, err := s.Store.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	// monitor feed: session started omitted; ended + idle + work stay
	feed := doJSON(t, wh, "GET", "/api/v1/worker/activities?worker=piso-worker-demo", nil)
	var acts []store.Activity
	_ = json.Unmarshal(feed.Body.Bytes(), &acts)
	if feed.Code != 200 || len(acts) != 3 {
		t.Fatalf("feed %d %d rows: %s", feed.Code, len(acts), feed.Body.String())
	}
	have := map[string]bool{}
	for _, a := range acts {
		have[a.Text] = true
		if a.Text == "session started" {
			t.Fatalf("session started leaked into feed: %s", feed.Body.String())
		}
	}
	for _, want := range []string{"real work", "idle at prompt", "session ended"} {
		if !have[want] {
			t.Fatalf("missing %q in %s", want, feed.Body.String())
		}
	}
	// host board keeps the Tier-A floor: started is still visible there
	ctrl := doJSON(t, s.ControlHandler(), "GET", "/api/v1/activities", nil)
	if ctrl.Code != 200 || !strings.Contains(ctrl.Body.String(), "session started") || !strings.Contains(ctrl.Body.String(), "session ended") {
		t.Fatalf("control feed %d %s", ctrl.Code, ctrl.Body.String())
	}
}

func TestWorkerActivityFeedTimeLimitedToJudgement(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	now := time.Now().UnixNano() / 1000000
	// demo history: old events … the monitor pokes demo … more work … then the
	// monitor curates demo (its NEWEST judgement for demo), and only work that
	// lands after THAT is post-judgement
	old := now - 3600 * 1000
	waiting := now - 1800 * 1000
	poke := now - 900 * 1000
	fresh := now - 600 * 1000
	curate := now - 120 * 1000
	newest := now - 60 * 1000
	// another project the monitor never judged: its old events stay visible
	other := now - 7200 * 1000
	for _, a := range []store.Activity{
		{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindProgress, Text: "old work", Ts: old},
		{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindWaiting, Text: "old waiting", Ts: waiting},
		{Worker: "piso-worker-monitor", Slug: "monitor", Kind: store.ActivityKindPoke, TargetSlug: "demo", Text: "human needed", Ts: poke},
		{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindProgress, Text: "fresh work", Ts: fresh},
		{Worker: "piso-worker-monitor", Slug: "monitor", Kind: store.ActivityKindNote, TargetSlug: "demo", Text: "curation", Ts: curate},
		{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindProgress, Text: "newest work", Ts: newest},
		{Worker: "piso-worker-other", Slug: "other", Kind: store.ActivityKindProgress, Text: "other work", Ts: other},
	} {
		if _, err := s.Store.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	// monitor reads its feed: demo's pre-judgement history is cut, everything
	// the monitor posted itself is retained, and unjudged projects are intact
	feed := doJSON(t, wh, "GET", "/api/v1/worker/activities?worker=piso-worker-monitor", nil)
	var acts []store.Activity
	_ = json.Unmarshal(feed.Body.Bytes(), &acts)
	if feed.Code != 200 || len(acts) != 4 {
		t.Fatalf("feed %d %d rows: %s", feed.Code, len(acts), feed.Body.String())
	}
	var haveOld bool
	have := map[string]bool{}
	for _, a := range acts {
		have[a.Text] = true
		if a.Text == "old work" || a.Text == "old waiting" || a.Text == "fresh work" {
			haveOld = true
		}
	}
	if haveOld {
		t.Fatalf("pre-judgement demo history still in feed: %s", feed.Body.String())
	}
	for _, want := range []string{"newest work", "human needed", "curation", "other work"} {
		if !have[want] {
			t.Fatalf("missing %q in %s", want, feed.Body.String())
		}
	}
	// demo's own (non-monitor) read keeps its full history: the derived window
	// only cuts rows for projects the REQUESTING worker judged, and demo never
	// aimed anything at a project (its map holds only its own ""), so nothing cuts
	own := doJSON(t, wh, "GET", "/api/v1/worker/activities?worker=piso-worker-demo", nil)
	var ownActs []store.Activity
	_ = json.Unmarshal(own.Body.Bytes(), &ownActs)
	if own.Code != 200 || len(ownActs) != 7 {
		t.Fatalf("demo own feed %d %d rows: %s", own.Code, len(ownActs), own.Body.String())
	}
	// explicit `since` overrides the derived window
	since := doJSON(t, wh, "GET", "/api/v1/worker/activities?worker=piso-worker-monitor&since="+strconv.FormatInt(fresh, 10), nil)
	var sinceActs []store.Activity
	_ = json.Unmarshal(since.Body.Bytes(), &sinceActs)
	if since.Code != 200 || len(sinceActs) != 3 {
		t.Fatalf("since feed %d %d rows: %s", since.Code, len(sinceActs), since.Body.String())
	}
	for _, a := range sinceActs {
		if a.Ts < fresh {
			t.Fatalf("row older than explicit since: %+v", a)
		}
	}
}

func TestWorkerActivityFeedTrackIncludesAimedPokes(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	now := time.Now().UnixNano() / 1000000
	// poke first, then the project works after being judged (pre-judgement
	// history is vetoed by the judgement window, so both surviving rows are
	// post-poke: the project's own work AND the poke aimed at it)
	for _, a := range []store.Activity{
		{Worker: "piso-worker-monitor", Slug: "monitor", Kind: store.ActivityKindPoke, TargetSlug: "demo", Text: "human needed", Ts: now - 60000},
		{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindProgress, Text: "real work", Ts: now - 30000},
		{Worker: "piso-worker-other", Slug: "other", Kind: store.ActivityKindProgress, Text: "other work", Ts: now},
	} {
		if _, err := s.Store.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	// the project track carries both its own events and pokes aimed at it,
	// newest first
	track := doJSON(t, wh, "GET", "/api/v1/worker/activities?worker=piso-worker-monitor&slug=demo", nil)
	var acts []store.Activity
	_ = json.Unmarshal(track.Body.Bytes(), &acts)
	if track.Code != 200 || len(acts) != 2 {
		t.Fatalf("track %d %d rows: %s", track.Code, len(acts), track.Body.String())
	}
	if acts[0].Text != "real work" || acts[1].Text != "human needed" || acts[1].TargetSlug != "demo" {
		t.Fatalf("track rows: %s", track.Body.String())
	}
	// kind filter applies to aimed rows too
	kf := doJSON(t, wh, "GET", "/api/v1/worker/activities?worker=piso-worker-monitor&slug=demo&kind=poke", nil)
	var kacts []store.Activity
	_ = json.Unmarshal(kf.Body.Bytes(), &kacts)
	if kf.Code != 200 || len(kacts) != 1 || kacts[0].Kind != store.ActivityKindPoke {
		t.Fatalf("kind filter %d %d rows: %s", kf.Code, len(kacts), kf.Body.String())
	}
	// a project nobody poked has only its own rows
	other := doJSON(t, wh, "GET", "/api/v1/worker/activities?worker=piso-worker-monitor&slug=other", nil)
	var oacts []store.Activity
	_ = json.Unmarshal(other.Body.Bytes(), &oacts)
	if other.Code != 200 || len(oacts) != 1 || oacts[0].Text != "other work" {
		t.Fatalf("other track %d %d rows: %s", other.Code, len(oacts), other.Body.String())
	}
}

func TestWorkerActivityFeedTrackDedupesSelfPoke(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	// a worker poking itself stores slug==targetSlug, so the row matches both
	// sides of the track query; it must still appear exactly once (work is
	// newer than the poke so the judgement window keeps it)
	for _, a := range []store.Activity{
		{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindPoke, TargetSlug: "demo", Text: "self poke", Ts: 1000},
		{Worker: "piso-worker-demo", Slug: "demo", Kind: store.ActivityKindProgress, Text: "work", Ts: 2000},
	} {
		if _, err := s.Store.InsertActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	track := doJSON(t, wh, "GET", "/api/v1/worker/activities?worker=piso-worker-demo&slug=demo", nil)
	var acts []store.Activity
	_ = json.Unmarshal(track.Body.Bytes(), &acts)
	if track.Code != 200 || len(acts) != 2 {
		t.Fatalf("track %d %d rows: %s", track.Code, len(acts), track.Body.String())
	}
}

func TestWorkerActivityValidation(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()
	// bad worker
	if w := doJSON(t, wh, "POST", "/api/v1/worker/activity", map[string]any{"worker": "../etc", "kind": "note", "text": "x"}); w.Code != 400 {
		t.Fatalf("bad worker %d", w.Code)
	}
	// unknown kind
	if w := doJSON(t, wh, "POST", "/api/v1/worker/activity", map[string]any{"worker": "piso-worker-demo", "kind": "bogus", "text": "x"}); w.Code != 400 {
		t.Fatalf("bad kind %d %s", w.Code, w.Body.String())
	}
	// empty text
	if w := doJSON(t, wh, "POST", "/api/v1/worker/activity", map[string]any{"worker": "piso-worker-demo", "kind": "note", "text": "  "}); w.Code != 400 {
		t.Fatalf("empty text %d", w.Code)
	}
	// control-plane read without worker (board) still works; worker feed needs a worker
	if w := doJSON(t, wh, "GET", "/api/v1/worker/activities", nil); w.Code != 400 {
		t.Fatalf("feed without worker %d", w.Code)
	}
}
