package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"piso/gateway/internal/store"
)

// WorkerHandler is the namespaced worker API. It is served on a separate
// listener (default :8083) that is not published to the host. Workers must
// not be given ControlHandler.
func (s *Server) WorkerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/worker/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "time": time.Now().UTC()})
	})
	mux.HandleFunc("GET /api/v1/worker/planning", s.handleWorkerGetPlanning)
	mux.HandleFunc("POST /api/v1/worker/planning", s.handleCreateIngress)
	mux.HandleFunc("POST /api/v1/worker/planning/cancel", s.handleCancelIngress)
	mux.HandleFunc("POST /api/v1/worker/context", s.handleWorkerPostContext)
	mux.HandleFunc("POST /api/v1/worker/checkin", s.handleWorkerCheckin)
	mux.HandleFunc("GET /api/v1/worker/capabilities", s.handleWorkerCapabilities)
	mux.HandleFunc("POST /api/v1/worker/ports", s.handleWorkerPostPorts)
	mux.HandleFunc("POST /api/v1/worker/activity", s.handleWorkerPostActivity)
	mux.HandleFunc("GET /api/v1/worker/activities", s.handleWorkerGetActivities)
	mux.HandleFunc("POST /api/v1/worker/activity/revoke", s.handleWorkerRevokeActivity)
	return mux
}

// portIn is one listener reported by the ports watcher.
type portIn struct {
	Port      int    `json:"port"`
	Reachable bool   `json:"reachable,omitempty"`
	Note      string `json:"note,omitempty"`
}

// portsIn is the watcher's full-set report: identity + every listening TCP
// port with reachability from the vpc.
type portsIn struct {
	Worker string   `json:"worker"`
	Slug   string   `json:"slug,omitempty"`
	Ports  []portIn `json:"ports"`
}

// handleWorkerPostPorts receives the ports watcher's listener set and
// auto-creates <slug>-<port> routes for the reachable ports (the expose loop
// in the plan: any server, any worker, no host ceremony). Origin marks routes
// as watcher-managed (rate-capped, GC'd after down-grace) vs host-created
// (sticky). Identity is checked against the slug↔IP registry so a worker
// cannot squat another worker's label namespace.
func (s *Server) handleWorkerPostPorts(w http.ResponseWriter, r *http.Request) {
	var in portsIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Worker = strings.TrimSpace(in.Worker)
	in.Slug = strings.TrimSpace(in.Slug)
	if !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "worker name required"})
		return
	}
	if in.Slug == "" {
		in.Slug = slugFromWorker(in.Worker)
	}
	if !workerNameRe.MatchString(in.Slug) {
		writeJSON(w, 400, map[string]string{"error": "slug required"})
		return
	}
	// A registered worker's source IP must match the reported identity.
	if reg, ok := s.Store.WorkerByIP(requestIP(r)); ok && reg.Name != in.Worker {
		identityMismatch(w, r, reg, in.Worker, in.Slug)
		return
	}
	ports := make([]int, 0, len(in.Ports))
	unreachable := make([]store.UnreachablePort, 0, len(in.Ports))
	seen := map[int]bool{}
	for _, p := range in.Ports {
		if p.Port < 1 || p.Port > 65535 || seen[p.Port] {
			continue
		}
		seen[p.Port] = true
		if p.Reachable {
			ports = append(ports, p.Port)
			continue
		}
		unreachable = append(unreachable, store.UnreachablePort{Port: p.Port, Note: sanitizeLabel(p.Note, 80)})
	}
	if _, err := s.Store.SyncWorkerPorts(in.Worker, in.Slug, ports); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.Store.SetWorkerUnreachable(in.Worker, unreachable)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// requestIP returns the caller's host IP (no port) for registry lookups.
func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host
}

// identityMismatch writes a 403 naming the registry's owner of the caller's
// source IP vs the claimed identity, so operator logs show exactly what to
// fix (re-run `piso up`, or redeploy the conflicting worker). piso-informant
// prints this body, so the agent line reads as an actionable message.
func identityMismatch(w http.ResponseWriter, r *http.Request, reg store.WorkerRec, name, slug string) {
	detail := fmt.Sprintf("registered %s@%s; request claims worker=%s",
		reg.Name, requestIP(r), name)
	if slug != "" {
		detail = detail + fmt.Sprintf(" slug=%s", slug)
	}
	writeJSON(w, 403, map[string]string{"error": "identity mismatch", "detail": detail})
}

// checkinIn is a worker's startup identity claim (self-healing registry).
type checkinIn struct {
	Worker string `json:"worker"`
	Slug   string `json:"slug,omitempty"`
}

// handleWorkerCheckin claims the caller's source IP for its own worker
// record, so a container recreate (new vpc IP) does not strand the registry
// with a stale mapping that 403s every activity/ports/context POST. The vpc
// bridge prevents source-IP spoofing and the claim is first-come (same trust
// model as the host CLI's `piso up` registration), so this cannot be used to
// squat another worker's identity:
//   - IP already claimed by THIS worker (name+slug) → no-op, 200.
//   - IP claimed by a DIFFERENT worker → 403 with detail (strict; the host
//     must redeploy the conflicting worker or re-run `piso up`).
//   - IP unclaimed → append the IP to this worker's record (merge, never
//     clobber the registered IP set) and return the authoritative record.
func (s *Server) handleWorkerCheckin(w http.ResponseWriter, r *http.Request) {
	var in checkinIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Worker = strings.TrimSpace(in.Worker)
	in.Slug = strings.TrimSpace(in.Slug)
	if !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "worker name required"})
		return
	}
	if in.Slug == "" {
		in.Slug = slugFromWorker(in.Worker)
	}
	if !workerNameRe.MatchString(in.Slug) {
		writeJSON(w, 400, map[string]string{"error": "slug required"})
		return
	}
	ip := requestIP(r)
	reg, regOK := s.Store.WorkerByIP(ip)
	if regOK && reg.Name == in.Worker && reg.Slug == in.Slug {
		writeCheckinOK(w, reg) // already registered correctly; nothing to heal
		return
	}
	if regOK {
		identityMismatch(w, r, reg, in.Worker, in.Slug)
		return
	}
	// IP unclaimed: append it to this worker's record, keeping any existing
	// IPs (UpsertWorker replaces the IP set keyed by name, so merge first).
	rec := store.WorkerRec{Name: in.Worker, Slug: in.Slug, IPs: make([]string, 0, 4)}
	if existing, ok := s.Store.WorkerByName(in.Worker); ok {
		rec.Dir = existing.Dir
		rec.InternetDisabled = existing.InternetDisabled
		for _, eip := range existing.IPs {
			if eip != ip {
				rec.IPs = append(rec.IPs, eip)
			}
		}
	}
	rec.IPs = append(rec.IPs, ip)
	out, err := s.Store.UpsertWorker(rec)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeCheckinOK(w, out)
}

// checkinOut is the worker-visible identity after a successful checkin.
// Host-only fields (Dir, InternetDisabled, UpdatedAt) stay off this
// response — Dir is the host project path and must not enter the worker.
type checkinOut struct {
	Name string   `json:"name"`
	Slug string   `json:"slug"`
	IPs  []string `json:"ips,omitempty"`
}

func writeCheckinOK(w http.ResponseWriter, rec store.WorkerRec) {
	writeJSON(w, 200, checkinOut{Name: rec.Name, Slug: rec.Slug, IPs: rec.IPs})
}

const (
	monitorSlug       = "monitor"
	monitorWorkerName = "piso-worker-monitor"
)

// isMonitorWorker reports the project-manager container, which is the only
// worker allowed to read the full activity board.
func isMonitorWorker(name, slug string) bool {
	return slug == monitorSlug || name == monitorWorkerName
}

// activityIn is one informant report (or monitor poke) posted by a worker.
type activityIn struct {
	Worker     string `json:"worker"`
	Slug       string `json:"slug,omitempty"`
	Kind       string `json:"kind"`
	TargetSlug string `json:"targetSlug,omitempty"`
	Text       string `json:"text"`
}

// handleWorkerPostActivity stores an activity reported by a worker's
// informant (or the monitor). The origin worker is identity-checked against
// the slug↔IP registry exactly like the ports handler; text is sanitized
// (labels render in the host dashboard). No headers/bodies ever enter the
// activity store — only the sanitized text.
func (s *Server) handleWorkerPostActivity(w http.ResponseWriter, r *http.Request) {
	var in activityIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Worker = strings.TrimSpace(in.Worker)
	in.Slug = strings.TrimSpace(in.Slug)
	in.Kind = strings.TrimSpace(in.Kind)
	in.TargetSlug = strings.TrimSpace(in.TargetSlug)
	in.Text = strings.TrimSpace(in.Text)
	if !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "worker name required"})
		return
	}
	if in.Slug == "" {
		in.Slug = slugFromWorker(in.Worker)
	}
	if !workerNameRe.MatchString(in.Slug) {
		writeJSON(w, 400, map[string]string{"error": "slug required"})
		return
	}
	if !store.ValidActivityKind(in.Kind) {
		writeJSON(w, 400, map[string]string{"error": "unknown kind"})
		return
	}
	if in.Text == "" {
		writeJSON(w, 400, map[string]string{"error": "text required"})
		return
	}
	// A registered worker's source IP must match the reported identity.
	if reg, ok := s.Store.WorkerByIP(requestIP(r)); ok && (reg.Name != in.Worker || reg.Slug != in.Slug) {
		identityMismatch(w, r, reg, in.Worker, in.Slug)
		return
	}
	a := store.Activity{
		Worker: in.Worker, Slug: in.Slug, Kind: in.Kind,
		TargetSlug: sanitizeLabel(in.TargetSlug, 63),
		Text:       sanitizeLabel(in.Text, 500),
	}
	stored, err := s.Store.InsertActivity(a)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, stored)
}

// handleWorkerCapabilities reports the worker's LIVE sandbox boundaries that
// the gateway is the source of truth for (static image facts — no ssh, no
// docker, read-only rootfs — are declared in the CAPABILITIES section of
// ~/.pi/agent/AGENTS.md instead). The agent fetches this at session start so
// it knows BEFORE attempting a step whether a boundary blocks it: if a
// capability is unclear or the fetch fails, treat it as BLOCKED and emit a
// `host` activity requesting the step instead of probing blindly.
func (s *Server) handleWorkerCapabilities(w http.ResponseWriter, r *http.Request) {
	worker := strings.TrimSpace(r.URL.Query().Get("worker"))
	if !workerNameRe.MatchString(worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	if reg, ok := s.Store.WorkerByIP(requestIP(r)); ok && reg.Name != worker {
		identityMismatch(w, r, reg, worker, "")
		return
	}
	internet := true
	slug := slugFromWorker(worker)
	if reg, ok := s.Store.WorkerByName(worker); ok {
		slug = reg.Slug
		internet = !reg.InternetDisabled
	}
	// Which placeholders does this worker's model actually see? Same rule as
	// the env-file renderer (SecretVisibleToWorker): empty/* Workers = all.
	var scoped []string
	for _, sec := range s.Store.Secrets() {
		if store.SecretVisibleToWorker(sec, slug) {
			scoped = append(scoped, sec.Placeholder)
		}
	}
	writeJSON(w, 200, map[string]any{
		"worker":             worker,
		"slug":               slug,
		"internetEnabled":    internet,
		"egressProxy":        "gateway:8080",
		"scopedPlaceholders": scoped,
		"outOfBound":         "emit a host activity request instead of attempting",
	})
}

// activityView is one feed row as the monitor/loop consume it: the stored
// activity plus read-time age fields, so judging never requires timestamp
// arithmetic. All rows of one response share the same `now`.
type activityView struct {
	store.Activity
	AgeMin   int    `json:"ageMin"`
	AgeLabel string `json:"ageLabel"`
}

// activityAge renders the age of tsMs against nowMs ("just now", "N min ago",
// "N h M min ago", "Nd ago") plus the floor of elapsed minutes.
func activityAge(tsMs, nowMs int64) (int, string) {
	if nowMs <= tsMs {
		return 0, "just now"
	}
	sec := (nowMs - tsMs) / 1000
	if sec < 90 {
		return 0, "just now"
	}
	min := sec / 60
	if min < 60 {
		return int(min), fmt.Sprintf("%d min ago", min)
	}
	h := min / 60
	if h < 24 {
		r := min % 60
		if r != 0 {
			return int(min), fmt.Sprintf("%d h %d min ago", h, r)
		}
		return int(min), fmt.Sprintf("%d h ago", h)
	}
	return int(min), fmt.Sprintf("%dd ago", h/24)
}

// feedView attaches the age fields to one feed read.
func feedView(rows []store.Activity, nowMs int64) []activityView {
	var out []activityView
	for _, a := range rows {
		m, lab := activityAge(a.Ts, nowMs)
		out = append(out, activityView{Activity: a, AgeMin: m, AgeLabel: lab})
	}
	return out
}

// activeSpanGapMs is the largest gap between consecutive beats that still
// counts as the same working run (matches the digest's break rule).
const activeSpanGapMs = 10 * 60 * 1000

// collapseActiveSpans fuses each worker's consecutive kind=active rows into
// one synthesized row per working run: same worker+slug+text with consecutive
// beats ≤ activeSpanGapMs apart. The synthesized row keeps the NEWEST beat's
// ts/id, and carries activeFromMs (run start) + activeBeats (beat count); its
// text gains " · for N min" for multi-beat runs. Read-side ONLY — nothing is
// deleted or rewritten; the store keeps every beat. Input is newest-first and
// the output preserves that order.
func collapseActiveSpans(rows []store.Activity) []store.Activity {
	out := make([]store.Activity, 0, len(rows))
	type run struct {
		act    store.Activity
		lastTs int64 // ts of the most recent beat processed (newest boundary)
		fromTs int64 // oldest beat ts in the run
		count  int
	}
	var cur *run
	flush := func() {
		if cur == nil {
			return
		}
		if cur.count > 1 || cur.act.Ts-cur.fromTs > 60000 {
			min := (cur.act.Ts - cur.fromTs) / 60000
			if min < 1 {
				min = 1
			}
			cur.act.Text = cur.act.Text + " · for " + strconv.FormatInt(min, 10) + " min"
			cur.act.ActiveFromMs = cur.fromTs
			cur.act.ActiveBeats = cur.count
		}
		out = append(out, cur.act)
		cur = nil
	}
	for _, a := range rows {
		if a.Kind != store.ActivityKindActive {
			flush()
			out = append(out, a)
			continue
		}
		if cur != nil && a.Worker == cur.act.Worker && a.Slug == cur.act.Slug && a.Text == cur.act.Text && (cur.lastTs-a.Ts) <= activeSpanGapMs {
			cur.count += 1
			cur.fromTs = a.Ts
			cur.lastTs = a.Ts
			continue
		}
		flush()
		cur = &run{act: a, lastTs: a.Ts, fromTs: a.Ts, count: 1}
	}
	flush()
	return out
}

// dropSessionStartedNotes removes the Tier-A watcher's "session started" notes.
// Those are noise for the monitor (a start is visible as the first active/idle
// row). "session ended" is KEPT: it is how the judge knows pi is gone, so an
// earlier idle is not left as the latest event.
func dropSessionStartedNotes(rows []store.Activity) []store.Activity {
	if len(rows) == 0 {
		return rows
	}
	out := rows[:0]
	for _, a := range rows {
		if a.Kind == store.ActivityKindNote && a.Text == "session started" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// limitToLastJudgment time-limits each project's rows to events at/after the
// requesting worker's last own posting aimed at that project (its judgement;
// last maps project slug → newest ts). Projects the worker never judged, and
// the worker's own rows (their slug is not a judgement target), are kept in
// full — so the monitor still sees its own poke history and only its judged
// projects' pre-judgement history is suppressed.
func limitToLastJudgment(rows []store.Activity, last map[string]int64) []store.Activity {
	if len(last) == 0 {
		return rows
	}
	out := rows[:0]
	for _, a := range rows {
		if t, ok := last[a.Slug]; ok && a.Ts < t {
			continue
		}
		out = append(out, a)
	}
	return out
}

// handleWorkerGetActivities returns the activity feed. The monitor (slug
// "monitor") polls the full board. Every other worker is scoped to its own
// project track (own slug + rows aimed at it). Caller identity is verified
// like the ports handler.
//
// Two read-side decisions shape the series of events returned (nothing is
// deleted; the store keeps full history):
//   - "session started" notes are omitted; "session ended" is kept (pi gone).
//   - Unless an explicit `since` is given, each project's rows are time-limited
//     to events newer than the requesting worker's last judgement aimed at that
//     project (the monitor never re-judges history it already acted on).
//   - `active=span` then fuses consecutive beats into one row per working run.
func (s *Server) handleWorkerGetActivities(w http.ResponseWriter, r *http.Request) {
	worker := strings.TrimSpace(r.URL.Query().Get("worker"))
	if !workerNameRe.MatchString(worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	if reg, ok := s.Store.WorkerByIP(requestIP(r)); ok && reg.Name != worker {
		identityMismatch(w, r, reg, worker, "")
		return
	}
	callerSlug := slugFromWorker(worker)
	if reg, ok := s.Store.WorkerByName(worker); ok && reg.Slug != "" {
		callerSlug = reg.Slug
	}
	f := store.ActivityFilter{
		Kind:  strings.TrimSpace(r.URL.Query().Get("kind")),
		Slug:  strings.TrimSpace(r.URL.Query().Get("slug")),
		Limit: atoiDefault(r.URL.Query().Get("limit"), 500),
	}
	// Non-monitor workers cannot read another project's track.
	if !isMonitorWorker(worker, callerSlug) {
		if f.Slug != "" && f.Slug != callerSlug {
			writeJSON(w, 403, map[string]string{"error": "slug out of scope"})
			return
		}
		f.Slug = callerSlug
	}
	// Explicit `since` (unix ms) overrides the derived judgement window.
	explicitSince := ""
	if v := strings.TrimSpace(r.URL.Query().Get("since")); v != "" {
		explicitSince = v
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			f.SinceMs = n
		}
	}
	acts, err := s.Store.QueryActivities(f)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// Track semantics for `slug=`: a project's timeline is not only its own
	// rows — pokes/reminders/notes AIMED at it (monitor-sourced rows carry
	// targetSlug, not the reporter's slug) belong on its track. The board
	// renders them there (targetSlug wins over slug); the feed must do the
	// same or a per-project read misses the pokes on its track.
	if f.Slug != "" {
		aimed, aerr := s.Store.QueryActivities(store.ActivityFilter{
			TargetSlug: f.Slug, Kind: f.Kind, SinceMs: f.SinceMs, Limit: f.Limit,
		})
		if aerr != nil {
			writeJSON(w, 500, map[string]string{"error": aerr.Error()})
			return
		}
		if len(aimed) > 0 {
			merged := make([]store.Activity, 0, len(acts)+len(aimed))
			seen := make(map[string]bool)
			i, j := 0, 0 // both queries return newest-first
			for i < len(acts) || j < len(aimed) {
				var p store.Activity
				if j >= len(aimed) || (i < len(acts) && acts[i].Ts >= aimed[j].Ts) {
					p = acts[i]
					i += 1
				} else {
					p = aimed[j]
					j += 1
				}
				if !seen[p.ID] { // a self-poke (slug==target) matches both sides
					seen[p.ID] = true
					merged = append(merged, p)
				}
			}
			if len(merged) > f.Limit {
				merged = merged[:f.Limit]
			}
			acts = merged
		}
	}
	if acts == nil {
		acts = []store.Activity{}
	}
	acts = dropSessionStartedNotes(acts)
	if explicitSince == "" {
		if last, err := s.Store.NewestTsByTarget(worker); err == nil && len(last) > 0 {
			acts = limitToLastJudgment(acts, last)
		}
	}
	// active=span collapses each worker's consecutive beats (same slug+text,
	// ≤ activeSpanGapMs apart) into one synthesized row per working run. This
	// is a READ projection: the store keeps every beat; the monitor gets the
	// compact view without timestamp arithmetic.
	if strings.TrimSpace(r.URL.Query().Get("active")) == "span" {
		acts = collapseActiveSpans(acts)
	}
	writeJSON(w, 200, feedView(acts, time.Now().UnixNano()/1000000))
}

// handleWorkerRevokeActivity removes one of the caller's own activity rows
// (id is scoped to the posting worker).
func (s *Server) handleWorkerRevokeActivity(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Worker string `json:"worker"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.ID = strings.TrimSpace(in.ID)
	in.Worker = strings.TrimSpace(in.Worker)
	if in.ID == "" || !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "id and worker required"})
		return
	}
	if reg, ok := s.Store.WorkerByIP(requestIP(r)); ok && reg.Name != in.Worker {
		identityMismatch(w, r, reg, in.Worker, "")
		return
	}
	deleted, err := s.Store.DeleteOwnActivity(in.ID, in.Worker)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if !deleted {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	w.WriteHeader(204)
}

// atoiDefault parses s as an int, returning def on empty/invalid.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return def
	}
	return n
}

// handleWorkerPostContext receives the watcher's live context and stores it.
// Worker-supplied and advisory; validated for shape and sanitized (labels end
// up rendered in the dashboard HTML). No semantic enforcement.
func (s *Server) handleWorkerPostContext(w http.ResponseWriter, r *http.Request) {
	var in store.WorkerCtx
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	in.Worker = strings.TrimSpace(in.Worker)
	in.Slug = strings.TrimSpace(in.Slug)
	if !workerNameRe.MatchString(in.Worker) {
		writeJSON(w, 400, map[string]string{"error": "worker name required"})
		return
	}
	if in.Slug == "" {
		in.Slug = slugFromWorker(in.Worker)
	}
	if !workerNameRe.MatchString(in.Slug) {
		writeJSON(w, 400, map[string]string{"error": "slug required"})
		return
	}
	// Sanitize label fields for the dashboard (strip control chars, cap).
	in.Folder = sanitizeLabel(in.Folder, 256)
	in.Project = sanitizeLabel(in.Project, 120)
	in.Branch = sanitizeLabel(in.Branch, 120)
	in.Commit = sanitizeLabel(in.Commit, 40)
	in.Model = sanitizeLabel(in.Model, 120)
	if _, err := s.Store.UpsertWorkerCtx(in); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleWorkerGetPlanning(w http.ResponseWriter, r *http.Request) {
	worker := strings.TrimSpace(r.URL.Query().Get("worker"))
	if !workerNameRe.MatchString(worker) {
		writeJSON(w, 400, map[string]string{"error": "worker required"})
		return
	}
	pending := s.Store.PendingIngressForWorker(worker)
	if len(pending) > 0 {
		writeJSON(w, 200, viewIngress(pending[0], false))
		return
	}
	slug := slugFromWorker(worker)
	name := planningRouteName(slug)
	if route, ok := s.Store.RouteByName(name); ok && route.Worker == worker {
		writeJSON(w, 200, ingressView{
			Kind: store.IngressKindPlanning, Status: "approved",
			Worker: worker, Slug: slug, Port: route.Port, Name: name,
			URL: ingressPublicURL(name),
		})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "none"})
}

// sanitizeLabel strips control characters (replacing with a space), collapses
// whitespace runs, and caps length so labels can be embedded safely in the
// dashboard HTML and request-log JSONL. Values are advisory.
func sanitizeLabel(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len(s) > max {
		s = s[:max]
	}
	return s
}
