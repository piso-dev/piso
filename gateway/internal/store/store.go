// Package store holds gateway state: secrets, rules, domains, exceptions,
// routes, and the request log. Secrets/rules/domains/exceptions persist to a
// host-mounted JSON file; the request log appends to JSONL. Real secret values
// are written to disk (gateway-only file) but never to the log.
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"piso/gateway/internal/patterns"
)

// CurrentStateVersion is written into state.json. Bump when adding a
// migration in applyMigrations.
const CurrentStateVersion = 1

// State is the persisted configuration.
type State struct {
	Version    int            `json:"version"`
	Secrets    []SecretRec    `json:"secrets"`
	Rules      []RuleRec      `json:"rules"`
	Domains    []DomainRec    `json:"domains"`
	Exceptions []ExceptionRec `json:"exceptions"`
	Routes     []RouteRec     `json:"routes"`
}

// JSON-friendly records (this package owns persistence shape).
type SecretRec struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Placeholder  string    `json:"placeholder"`
	Value        string    `json:"value"`
	AllowedHosts []string  `json:"allowedHosts,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

type RuleRec struct {
	ID          string `json:"id"`
	SecretID    string `json:"secretId"`
	Host        string `json:"host"`
	Placeholder string `json:"placeholder"`
	Note        string `json:"note,omitempty"`
}

type DomainRec struct {
	Host      string `json:"host"`
	Deny      bool   `json:"deny"`
	NeedsRule bool   `json:"needsRule"`
	Note      string `json:"note,omitempty"`
}

type ExceptionRec struct {
	ID           string `json:"id"`
	Note         string `json:"note,omitempty"`
	HostRegex    string `json:"hostRegex,omitempty"`
	PathRegex    string `json:"pathRegex,omitempty"`
	ContentRegex string `json:"contentRegex,omitempty"`
	Placeholder  string `json:"placeholder,omitempty"`
	Enabled      bool   `json:"enabled"`
}

type RouteRec struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Worker string `json:"worker"`
	Port   int    `json:"port"`
	Note   string `json:"note,omitempty"`
}

// Store is the thread-safe in-memory + on-disk state manager.
type Store struct {
	mu           sync.RWMutex
	state        State
	path         string // config file path
	logPath      string // request log path
	patternsPath string // patterns file path

	// live request log
	records  []Record // most recent first
	maxLog   int
	onRecord chan Record // broadcast for SSE
}

// Record is the log-safe request record (this package's persistence/live
// shape; the model has the API shape — the server converts).
type Record struct {
	ID        string    `json:"id"`
	Worker    string    `json:"worker"`
	Ts        time.Time `json:"ts"`
	Method    string    `json:"method"`
	Scheme    string    `json:"scheme"`
	Host      string    `json:"host"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	Action    string    `json:"action"`
	Reasons   []string  `json:"reasons"`
	Findings  []Finding `json:"findings"`
	RequestID string    `json:"requestId"`
	Retryable bool      `json:"retryable"`
	// Capture holds the raw request for replay. It is server-side only and
	// NEVER serialized (json:"-"): the request log/SSE/API must not contain
	// raw bodies or headers (they can hold real credentials on a real-secret
	// block). Retry reads it from the in-memory ring.
	Capture *Capture `json:"-"`
}

type Finding struct {
	Kind      string `json:"kind"`
	PatternID string `json:"patternId,omitempty"`
	SecretID  string `json:"secretId,omitempty"`
	Token     string `json:"token,omitempty"`
	Location  string `json:"location"`
	Field     string `json:"field,omitempty"`
}

type Capture struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body"`
}

// New loads state from path (creating it if missing) and prepares the log.
func New(path, logPath, patternsPath string, maxLog int) (*Store, error) {
	s := &Store{
		path: path, logPath: logPath, patternsPath: patternsPath, maxLog: maxLog,
		onRecord: make(chan Record, 64),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s.save()
		}
		return err
	}
	if len(data) == 0 {
		return s.save()
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("state file %s: %w", s.path, err)
	}
	if applyMigrations(&st) {
		s.state = st
		return s.save()
	}
	s.state = st
	return nil
}

func (s *Store) save() error {
	if s.state.Version == 0 {
		s.state.Version = CurrentStateVersion
	}
	data, err := json.MarshalIndent(&s.state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---- reads ----

func (s *Store) Secrets() []SecretRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyRecs(s.state.Secrets)
}
func (s *Store) Rules() []RuleRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyRuleRecs(s.state.Rules)
}
func (s *Store) Domains() []DomainRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyDomainRecs(s.state.Domains)
}
func (s *Store) Exceptions() []ExceptionRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyExceptionRecs(s.state.Exceptions)
}
func (s *Store) Routes() []RouteRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyRouteRecs(s.state.Routes)
}
func (s *Store) RouteByName(name string) (RouteRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.Routes {
		if r.Name == name {
			return r, true
		}
	}
	return RouteRec{}, false
}

// ---- mutations (all persist) ----

func (s *Store) AddSecret(rec SecretRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Secrets = append(s.state.Secrets, rec)
	return s.save()
}

func (s *Store) DeleteSecret(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state.Secrets[:0]
	for _, r := range s.state.Secrets {
		if r.ID != id {
			out = append(out, r)
		}
	}
	s.state.Secrets = out
	// drop rules referencing it
	s.state.Rules = filterRules(s.state.Rules, func(r RuleRec) bool { return r.SecretID != id })
	return s.save()
}

func (s *Store) AddRule(rec RuleRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Rules = append(s.state.Rules, rec)
	return s.save()
}

func (s *Store) DeleteRule(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Rules = filterRules(s.state.Rules, func(r RuleRec) bool { return r.ID != id })
	return s.save()
}

func (s *Store) UpsertDomain(rec DomainRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, d := range s.state.Domains {
		if d.Host == rec.Host {
			s.state.Domains[i] = rec
			return s.save()
		}
	}
	s.state.Domains = append(s.state.Domains, rec)
	return s.save()
}

func (s *Store) DeleteDomain(host string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state.Domains[:0]
	for _, d := range s.state.Domains {
		if d.Host != host {
			out = append(out, d)
		}
	}
	s.state.Domains = out
	return s.save()
}

func (s *Store) AddException(rec ExceptionRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Exceptions = append(s.state.Exceptions, rec)
	return s.save()
}

// ---- patterns (separate file, relocatable to a compiled library) ----

// LoadPatterns loads the merged compiled library from the patterns file.
func (s *Store) LoadPatterns() (*patterns.Compiled, error) {
	return patterns.Load(s.patternsPath)
}

// AddOrReplacePattern upserts a pattern into the patterns file, preserving
// entries whose IDs aren't touched.
func (s *Store) AddOrReplacePattern(pat patterns.Entry) error {
	entries, err := s.readPatterns()
	if err != nil {
		return err
	}
	replaced := false
	for i, e := range entries {
		if e.ID == pat.ID {
			entries[i] = pat
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, pat)
	}
	return s.writePatterns(entries)
}

// DeletePattern removes a custom pattern (defaults can't be deleted — they
// are re-added by the library merge; deleting a default is a no-op with a
// disabled override being the supported mechanism).
func (s *Store) DeletePattern(id string) error {
	entries, err := s.readPatterns()
	if err != nil {
		return err
	}
	out := entries[:0]
	for _, e := range entries {
		if e.ID != id {
			out = append(out, e)
		}
	}
	return s.writePatterns(out)
}

func (s *Store) readPatterns() ([]patterns.Entry, error) {
	data, err := os.ReadFile(s.patternsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []patterns.Entry
	if len(data) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) writePatterns(entries []patterns.Entry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.patternsPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.patternsPath, data, 0o600)
}

func (s *Store) DeleteException(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state.Exceptions[:0]
	for _, e := range s.state.Exceptions {
		if e.ID != id {
			out = append(out, e)
		}
	}
	s.state.Exceptions = out
	return s.save()
}

func (s *Store) AddRoute(rec RouteRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.state.Routes {
		if r.Name == rec.Name {
			s.state.Routes[i] = rec
			return s.save()
		}
	}
	s.state.Routes = append(s.state.Routes, rec)
	return s.save()
}

func (s *Store) DeleteRoute(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state.Routes[:0]
	for _, r := range s.state.Routes {
		if r.ID != id {
			out = append(out, r)
		}
	}
	s.state.Routes = out
	return s.save()
}

// ---- request log ----

// AppendLog records a request and broadcasts it to SSE subscribers.
func (s *Store) AppendLog(rec Record) {
	s.mu.Lock()
	s.records = append([]Record{rec}, s.records...)
	if len(s.records) > s.maxLog {
		s.records = s.records[:s.maxLog]
	}
	s.mu.Unlock()
	// persist JSONL (best-effort; log loss is acceptable, policy is not)
	if f, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		if data, err := json.Marshal(rec); err == nil {
			f.Write(append(data, '\n'))
		}
		f.Close()
	}
	// broadcast (non-blocking; drop if no subscriber is reading)
	select {
	case s.onRecord <- rec:
	default:
	}
}

// Records returns the recent log, newest first.
func (s *Store) Records(limit int) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > len(s.records) {
		limit = len(s.records)
	}
	out := make([]Record, limit)
	copy(out, s.records[:limit])
	return out
}

// Sub returns a channel of new records (SSE). Callers must drain.
func (s *Store) Sub() <-chan Record { return s.onRecord }

// ReplayGet returns a captured record by id (for retry).
func (s *Store) ReplayGet(id string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.records {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

// ReplayUpdate replaces the record after a replay with the new outcome.
func (s *Store) ReplayUpdate(rec Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.records {
		if r.ID == rec.ID {
			s.records[i] = rec
			return
		}
	}
	s.records = append([]Record{rec}, s.records...)
	if len(s.records) > s.maxLog {
		s.records = s.records[:s.maxLog]
	}
}

// ---- copy helpers (defensive: callers can't mutate store state) ----

func copyRecs(in []SecretRec) []SecretRec {
	out := make([]SecretRec, len(in))
	copy(out, in)
	return out
}
func copyRuleRecs(in []RuleRec) []RuleRec { out := make([]RuleRec, len(in)); copy(out, in); return out }
func copyDomainRecs(in []DomainRec) []DomainRec {
	out := make([]DomainRec, len(in))
	copy(out, in)
	return out
}
func copyExceptionRecs(in []ExceptionRec) []ExceptionRec {
	out := make([]ExceptionRec, len(in))
	copy(out, in)
	return out
}
func copyRouteRecs(in []RouteRec) []RouteRec {
	out := make([]RouteRec, len(in))
	copy(out, in)
	return out
}

func filterRules(in []RuleRec, keep func(RuleRec) bool) []RuleRec {
	out := in[:0]
	for _, r := range in {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// ScanLog replicates reading the persisted JSONL (limited use: full log tail
// after restart). Kept minimal — the in-memory ring is authoritative for the
// running gateway.
func ScanLog(path string, limit int, visit func(Record)) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var lines []Record
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		lines = append(lines, r)
	}
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	for _, r := range lines {
		visit(r)
	}
	return sc.Err()
}
