package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no CGO)
)

// Activity kinds emitted by worker informants and the monitor. The board
// color-codes by kind.
const (
	ActivityKindProgress  = "progress"  // work in progress, descriptive
	ActivityKindMilestone = "milestone" // a milestone reached
	ActivityKindReminder  = "reminder"  // a time-boxed commitment
	ActivityKindPoke      = "poke"      // a nudge (monitor → target project)
	ActivityKindActive    = "active"    // watcher heartbeat: worker is alive
	ActivityKindWaiting   = "waiting"   // agent run ended; human input needed
	ActivityKindIdle      = "idle"      // Tier A: pi is up but not working (at the prompt)
	ActivityKindHost      = "host"      // host action required (sandbox boundary)
	ActivityKindNote      = "note"      // unstructured note
)

// ValidActivityKind reports whether k is an accepted activity kind.
func ValidActivityKind(k string) bool {
	switch k {
	case ActivityKindProgress, ActivityKindMilestone, ActivityKindReminder,
		ActivityKindPoke, ActivityKindActive, ActivityKindWaiting,
		ActivityKindIdle, ActivityKindHost, ActivityKindNote:
		return true
	}
	return false
}

// Activity is one semantic event reported by a worker's informant (or by the
// monitor on behalf of another project). It is the unit the PM board renders
// on a project track. Stored in SQLite (activities.db): durable, queryable,
// and out-of-band from state.json (so adding activities needs no state
// migration). The store keeps FULL history — surfaces (monitor feed, loop
// digest, board) compress/aggregate at read time, never here.
type Activity struct {
	ID         string `json:"id"`
	Worker     string `json:"worker"`   // container name of the reporter
	Slug       string `json:"slug"`     // project slug of the reporter
	Kind       string `json:"kind"`
	TargetSlug string `json:"targetSlug,omitempty"` // for pokes/reminders: which project's track
	Text       string `json:"text"`
	Ts         int64  `json:"ts"`      // unix ms
	// ActiveFromMs / ActiveBeats are READ-side decorations: the worker feed's
	// active=span projection fills them when it fuses a run of beats into one
	// synthesized row. Never persisted.
	ActiveFromMs int64 `json:"activeFromMs,omitempty"`
	ActiveBeats  int   `json:"activeBeats,omitempty"`
}

// ActivityFilter narrows QueryActivities. Empty fields are not filters.
type ActivityFilter struct {
	Slug       string
	TargetSlug string
	Kind       string
	SinceMs    int64
	Limit      int
}

// activitySchema is idempotent SQL run when the activity DB is opened. The
// two indexes back the board's per-track and per-target queries.
const activitySchema = `
CREATE TABLE IF NOT EXISTS activities (
	id          TEXT PRIMARY KEY,
	worker      TEXT NOT NULL,
	slug        TEXT NOT NULL,
	kind        TEXT NOT NULL,
	target_slug TEXT,
	text        TEXT NOT NULL,
	ts          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_activities_slug_ts     ON activities(slug, ts);
CREATE INDEX IF NOT EXISTS idx_activities_target_ts   ON activities(target_slug, ts);
`

// openActivityDB opens (and initializes) the SQLite activity database at path.
func (s *Store) openActivityDB(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// busy_timeout + WAL keep concurrent SSE readers from hitting SQLITE_BUSY
	// under the single writer; MaxOpenConns(1) avoids writer contention.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(activitySchema); err != nil {
		db.Close()
		return fmt.Errorf("activity schema: %w", err)
	}
	s.activitiesDB = db
	return nil
}

// Close releases the activity DB (gateway shutdown / tests).
func (s *Store) Close() error {
	if s.activitiesDB != nil {
		return s.activitiesDB.Close()
	}
	return nil
}

// InsertActivity stores one activity and broadcasts it to SSE subscribers.
// Ts/ID/Kind defaults are filled when absent. Returns the stored row (with
// its server-assigned ID) so callers can hand it back (e.g. the POST response).
func (s *Store) InsertActivity(a Activity) (Activity, error) {
	if a.ID == "" {
		a.ID = "act_" + randID()
	}
	if a.Ts == 0 {
		a.Ts = time.Now().UnixNano() / 1000000
	}
	if a.Kind == "" {
		a.Kind = ActivityKindNote
	}
	if !ValidActivityKind(a.Kind) {
		return Activity{}, fmt.Errorf("unknown activity kind %q", a.Kind)
	}
	_, err := s.activitiesDB.Exec(
		`INSERT INTO activities (id, worker, slug, kind, target_slug, text, ts) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Worker, a.Slug, a.Kind, a.TargetSlug, a.Text, a.Ts,
	)
	if err != nil {
		return Activity{}, err
	}
	s.broadcastActivity(a)
	return a, nil
}

// QueryActivities returns activities matching f, newest first, limited by
// f.Limit (0 = no limit — callers should set one for the board).
func (s *Store) QueryActivities(f ActivityFilter) ([]Activity, error) {
	q := `SELECT id, worker, slug, kind, COALESCE(target_slug,''), text, ts FROM activities`
	var conds []string
	var args []any
	if f.Slug != "" {
		conds = append(conds, "slug = ?")
		args = append(args, f.Slug)
	}
	if f.TargetSlug != "" {
		conds = append(conds, "target_slug = ?")
		args = append(args, f.TargetSlug)
	}
	if f.Kind != "" {
		conds = append(conds, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.SinceMs > 0 {
		conds = append(conds, "ts >= ?")
		args = append(args, f.SinceMs)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY ts DESC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.activitiesDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.Worker, &a.Slug, &a.Kind, &a.TargetSlug, &a.Text, &a.Ts); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// NewestTsByTarget returns, per target_slug, the newest ts of any activity
// posted by worker (rows with no target map to ""). The worker feed uses
// this to time-limit each project's rows to events since the requesting
// worker's last posting aimed at that project — for the monitor, its last
// judgement (poke/note/reminder) about the project. Nothing is deleted; this
// is a read-side window like the span projection below.
func (s *Store) NewestTsByTarget(worker string) (map[string]int64, error) {
	rows, err := s.activitiesDB.Query(
		`SELECT COALESCE(target_slug, ''), MAX(ts) FROM activities WHERE worker = ? GROUP BY COALESCE(target_slug, '')`,
		worker,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var target string
		var ts int64
		if err := rows.Scan(&target, &ts); err != nil {
			return nil, err
		}
		out[target] = ts
	}
	return out, rows.Err()
}

// DeleteOwnActivity removes one row, scoped to the worker that wrote it.
// Returns whether a row was actually deleted.
func (s *Store) DeleteOwnActivity(id, worker string) (bool, error) {
	res, err := s.activitiesDB.Exec(
		`DELETE FROM activities WHERE id = ? AND worker = ?`, id, worker,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// broadcastActivity notifies SSE subscribers of a new activity. Same
// drop-if-full policy as the other broadcasts; the dashboard polls as a
// fallback.
func (s *Store) broadcastActivity(a Activity) {
	select {
	case s.onActivity <- a:
	default:
	}
}

// SubActivities returns a channel of new activities (SSE). Callers must drain.
func (s *Store) SubActivities() <-chan Activity { return s.onActivity }