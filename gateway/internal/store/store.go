// Package store holds gateway state: secrets, rules, domains, exceptions,
// routes, and the request log. Secrets/rules/domains/exceptions persist to a
// host-mounted JSON file; the request log appends to JSONL. Real secret values
// are written to disk (gateway-only file) but never to the log.
package store

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"piso/gateway/internal/patterns"
)

// CurrentStateVersion is written into state.json. Bump when adding a
// migration in applyMigrations.
const CurrentStateVersion = 4

// State is the persisted configuration.
type State struct {
	Version         int                 `json:"version"`
	Secrets         []SecretRec         `json:"secrets"`
	Rules           []RuleRec           `json:"rules"`
	Domains         []DomainRec         `json:"domains"`
	Exceptions      []ExceptionRec      `json:"exceptions"`
	Routes          []RouteRec          `json:"routes"`
	IngressRequests []IngressRequestRec `json:"ingressRequests,omitempty"`
	Workers         []WorkerRec         `json:"workers,omitempty"`
	Contexts        []WorkerCtx         `json:"contexts,omitempty"`
}

// JSON-friendly records (this package owns persistence shape).
type SecretRec struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Placeholder  string    `json:"placeholder"`
	Value        string    `json:"value"`
	EnvKey       string    `json:"envKey,omitempty"`
	AllowedHosts []string  `json:"allowedHosts,omitempty"`
	Workers      []string  `json:"workers,omitempty"` // empty or "*" = all workers
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
	PatternID    string `json:"patternId,omitempty"`
	Enabled      bool   `json:"enabled"`
}

type RouteRec struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Worker string `json:"worker"`
	Port   int    `json:"port"`
	Note   string `json:"note,omitempty"`
	// Origin is how the route was created: "expose" (host, sticky — survives
	// GC) or "auto" (worker ports watcher, <slug>-<port>, rate-capped and
	// swept when stale). Legacy / migrated routes default to "expose".
	Origin string `json:"origin,omitempty"`
	// LastSeenMs is the last watcher heartbeat that included this port
	// (unix ms). Auto routes only; drives the down badge and stale GC.
	LastSeenMs int64 `json:"lastSeenMs,omitempty"`
	// Disabled blocks serving this route (the gateway refuses to proxy it)
	// but KEEPS the row so the toggle can re-enable it. Auto routes that are
	// disabled are still swept by the stale-GC (they don't resurrect).
	Disabled bool `json:"disabled,omitempty"`
}

// WorkerRec is the slug↔IP registry populated by the host CLI at `piso up`.
// The proxy resolves a request's origin IP to a worker slug via IPs, so logs
// are marked with the project slug rather than a generic "worker".
type WorkerRec struct {
	Name             string    `json:"name"`    // container name, e.g. "piso-worker-demo"
	Slug             string    `json:"slug"`    // project slug, e.g. "demo"
	Dir              string    `json:"dir,omitempty"` // host project dir mounted at /workspace
	IPs              []string  `json:"ips,omitempty"` // vpc IP addresses of the container
	InternetDisabled bool      `json:"internetDisabled,omitempty"` // true = gateway refuses egress
	UpdatedAt        time.Time `json:"updatedAt"`
}

// WorkerCtx is the worker's live context, reported by piso-context-watch and
// snapshotted onto each request-log row by AppendLog (labels describe the
// request's origin worker at that moment; advisory only).
type WorkerCtx struct {
	Worker  string    `json:"worker"`     // container name
	Slug    string    `json:"slug"`       // project slug (unique key)
	Folder  string    `json:"folder,omitempty"`  // host path / worker cwd, e.g. /workspace
	Project string    `json:"project,omitempty"` // repo name or folder basename
	Branch  string    `json:"branch,omitempty"`  // git branch, "" when detached/absent
	Commit  string    `json:"commit,omitempty"`  // short sha
	Model   string    `json:"model,omitempty"`   // provider/modelId
	Ts      time.Time `json:"ts"`
}

// Ingress request statuses. Only pending rows appear in the dashboard inbox.
const (
	IngressKindPlanning  = "planning"
	IngressStatusPending = "pending"
	DefaultPlanningPort  = 19432
)

// IngressRequestRec is a worker asking the host to publish an ingress route.
// The worker cannot create a live route; the host approves from the dashboard.
type IngressRequestRec struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Worker    string    `json:"worker"`
	Slug      string    `json:"slug,omitempty"`
	Port      int       `json:"port"`
	Name      string    `json:"name"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Store is the thread-safe in-memory + on-disk state manager.
type Store struct {
	mu           sync.RWMutex
	state        State
	path         string // config file path
	logPath      string // request log path
	patternsPath string // patterns file path

	// activity DB (SQLite; out-of-band from state.json)
	activitiesDB *sql.DB

	// live request log
	records  []Record // most recent first
	maxLog   int
	onRecord chan Record // broadcast for SSE

	// pending ingress requests (worker-published plan UIs)
	onIngress chan IngressRequestRec // broadcast for SSE

	// activity events (informant reports + monitor pokes)
	onActivity chan Activity // broadcast for SSE

	// route changes (auto-published dev servers + host routes)
	onRoute chan RouteRec // broadcast for SSE

	// transient listener hints: worker → ports the watcher reported but the
	// gateway cannot route to (loopback / specific-IP / v6-only binds)
	unreachable map[string][]UnreachablePort
}

// Record is the log-safe request record (this package's persistence/live
// shape; the model has the API shape — the server converts).
type Record struct {
	ID        string    `json:"id"`
	Worker    string    `json:"worker"`
	Slug      string    `json:"slug,omitempty"`
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
	// Worker-context labels snapshotted from WorkerCtx at record time
	// (folder/path, git repo, branch, commit, pi model). Enriched in
	// AppendLog when the row doesn't already carry a label.
	Project string `json:"project,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Model   string `json:"model,omitempty"`
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

// New loads state from path (creating it if missing), prepares the log, and
// opens the SQLite activity database (activities.db). Activities are durable
// and queryable but live outside state.json, so they never trigger a state
// migration.
func New(path, logPath, patternsPath, activitiesPath string, maxLog int) (*Store, error) {
	s := &Store{
		path: path, logPath: logPath, patternsPath: patternsPath, maxLog: maxLog,
		onRecord:    make(chan Record, 64),
		onIngress:   make(chan IngressRequestRec, 64),
		onRoute:     make(chan RouteRec, 64),
		onActivity:  make(chan Activity, 64),
		unreachable: map[string][]UnreachablePort{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	if activitiesPath != "" {
		if err := s.openActivityDB(activitiesPath); err != nil {
			return nil, err
		}
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
// UnreachablePort is a listener the worker reported but the gateway cannot
// route to (loopback / specific-IP / IPv6-only bind). Advisory UI hint only;
// never persisted.
type UnreachablePort struct {
	Port int    `json:"port"`
	Note string `json:"note,omitempty"`
}

func (s *Store) Routes() []RouteRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Lazy GC: auto routes are only kept alive by the watcher heartbeat. A
	// port that stopped being reported (or a worker that died entirely)
	// leaves its route stale; sweep it after the down-grace. Expose routes
	// never expire. Deletions are broadcast so `piso sync --watch` and the
	// dashboard drop them promptly.
	out := s.state.Routes[:0]
	changed := false
	now := time.Now().UnixNano()/1000000
	for _, r := range s.state.Routes {
		if r.Origin == AutoRouteOriginAuto && r.LastSeenMs > 0 && now - r.LastSeenMs > AutoRouteGraceMs {
			changed = true
			s.broadcastRoute(r)
			continue
		}
		out = append(out, r)
	}
	if changed {
		s.state.Routes = out
		_ = s.save()
	}
	return out
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

func (s *Store) PendingIngress() []IngressRequestRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []IngressRequestRec
	for _, r := range s.state.IngressRequests {
		if r.Status == IngressStatusPending {
			out = append(out, r)
		}
	}
	return out
}

func (s *Store) PendingIngressForWorker(worker string) []IngressRequestRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []IngressRequestRec
	for _, r := range s.state.IngressRequests {
		if r.Status == IngressStatusPending && r.Worker == worker {
			out = append(out, r)
		}
	}
	return out
}

func (s *Store) IngressByID(id string) (IngressRequestRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.IngressRequests {
		if r.ID == id {
			return r, true
		}
	}
	return IngressRequestRec{}, false
}

func (s *Store) IngressRequests() []IngressRequestRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyIngressRecs(s.state.IngressRequests)
}

// ---- mutations (all persist) ----

func (s *Store) AddSecret(rec SecretRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Secrets = append(s.state.Secrets, rec)
	if err := s.save(); err != nil {
		return err
	}
	return s.writePlaceholdersEnvLocked()
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
	if err := s.save(); err != nil {
		return err
	}
	return s.writePlaceholdersEnvLocked()
}

// SecretByEnvKey returns the first secret with this env key.
func (s *Store) SecretByEnvKey(envKey string) (SecretRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rec := range s.state.Secrets {
		if rec.EnvKey != "" && rec.EnvKey == envKey {
			return rec, true
		}
	}
	return SecretRec{}, false
}

// SecretByPlaceholder returns the first secret with this placeholder.
func (s *Store) SecretByPlaceholder(placeholder string) (SecretRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rec := range s.state.Secrets {
		if rec.Placeholder == placeholder {
			return rec, true
		}
	}
	return SecretRec{}, false
}

// SetEnvKey updates the worker env name for a secret and rewrites placeholders.env.
func (s *Store) SetEnvKey(id, envKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, rec := range s.state.Secrets {
		if rec.ID != id {
			continue
		}
		s.state.Secrets[i].EnvKey = envKey
		if err := s.save(); err != nil {
			return err
		}
		return s.writePlaceholdersEnvLocked()
	}
	return fmt.Errorf("secret %s not found", id)
}

// ReplaceSecret overwrites a secret by ID and rewrites worker env files.
func (s *Store) ReplaceSecret(rec SecretRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.state.Secrets {
		if existing.ID != rec.ID {
			continue
		}
		s.state.Secrets[i] = rec
		if err := s.save(); err != nil {
			return err
		}
		return s.writePlaceholdersEnvLocked()
	}
	return fmt.Errorf("secret %s not found", rec.ID)
}

// HasRule reports whether placeholder already has a rule for host or "*".
func (s *Store) HasRule(placeholder, host string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.Rules {
		if r.Placeholder == placeholder && (r.Host == host || r.Host == "*") {
			return true
		}
	}
	return false
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
			if err := s.save(); err != nil {
				return err
			}
			s.broadcastRoute(rec)
			return nil
		}
	}
	s.state.Routes = append(s.state.Routes, rec)
	if err := s.save(); err != nil {
		return err
	}
	s.broadcastRoute(rec)
	return nil
}

func (s *Store) DeleteRoute(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state.Routes[:0]
	for _, r := range s.state.Routes {
		if r.ID != id {
			out = append(out, r)
			continue
		}
		s.broadcastRoute(r)
	}
	s.state.Routes = out
	return s.save()
}

// SetRouteDisabled flips the disable flag on a route (blocks proxy service,
// keeps the row so it can be re-enabled). Unknown id → ErrRouteNotFound.
func (s *Store) SetRouteDisabled(id string, disabled bool) (RouteRec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.state.Routes {
		if r.ID != id {
			continue
		}
		s.state.Routes[i].Disabled = disabled
		if err := s.save(); err != nil {
			return RouteRec{}, err
		}
		s.broadcastRoute(s.state.Routes[i])
		return s.state.Routes[i], nil
	}
	return RouteRec{}, ErrRouteNotFound
}

// SetWorkerUnreachable replaces the transient listener hints for a worker
// (the watcher posts the full set each time, so: wholesale replace).
func (s *Store) SetWorkerUnreachable(worker string, ports []UnreachablePort) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unreachable[worker] = ports
}

// WorkerUnreachable returns the transient listener hints for a worker.
func (s *Store) WorkerUnreachable(worker string) ([]UnreachablePort, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.unreachable[worker]; ok {
		return v, true
	}
	return nil, false
}

// ---- worker registry (IP→slug, populated by the host CLI at `piso up`) ----

// Workers returns a copy of the worker registry.
func (s *Store) Workers() []WorkerRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyWorkerRecs(s.state.Workers)
}

// UpsertWorker records or refreshes a worker's slug + vpc IPs, keyed by name.
func (s *Store) UpsertWorker(rec WorkerRec) (WorkerRec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.UpdatedAt = time.Now().UTC()
	for i, r := range s.state.Workers {
		if r.Name == rec.Name {
			// preserve the prior slug if the new one is empty; keep IPs updated
			if rec.Slug == "" {
				rec.Slug = r.Slug
			}
			s.state.Workers[i] = rec
			if err := s.save(); err != nil {
				return WorkerRec{}, err
			}
			return rec, nil
		}
	}
	s.state.Workers = append(s.state.Workers, rec)
	if err := s.save(); err != nil {
		return WorkerRec{}, err
	}
	return rec, nil
}

// WorkerByIP returns the worker whose vpc IPs include ip (exact string match).
func (s *Store) WorkerByIP(ip string) (WorkerRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.Workers {
		for _, wip := range r.IPs {
			if wip == ip {
				return r, true
			}
		}
	}
	return WorkerRec{}, false
}

// WorkerByName returns the registry entry for a container name.
func (s *Store) WorkerByName(name string) (WorkerRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.Workers {
		if r.Name == name {
			return r, true
		}
	}
	return WorkerRec{}, false
}

// WorkerBySlug returns the registry entry for a project slug.
func (s *Store) WorkerBySlug(slug string) (WorkerRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.Workers {
		if r.Slug == slug {
			return r, true
		}
	}
	return WorkerRec{}, false
}

// SetWorkerInternet flips the internet kill-switch for a worker, preserving
// its slug and IPs. Returns ErrWorkerNotFound if the worker isn't registered
// (the flag is meaningless without a registry entry to enforce it).
func (s *Store) SetWorkerInternet(name string, disabled bool) (WorkerRec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.state.Workers {
		if r.Name != name {
			continue
		}
		r.InternetDisabled = disabled
		r.UpdatedAt = time.Now().UTC()
		s.state.Workers[i] = r
		if err := s.save(); err != nil {
			return WorkerRec{}, err
		}
		return r, nil
	}
	return WorkerRec{}, ErrWorkerNotFound
}

// UpsertWorkerCtx records the worker's live context (reported by the in-
// container watcher). Keyed by slug; the watcher only posts on change.
// Returns the stored row and the Ts (for the dashboard).
func (s *Store) UpsertWorkerCtx(ctx WorkerCtx) (WorkerCtx, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx.Ts = time.Now().UTC()
	for i, c := range s.state.Contexts {
		if c.Slug == ctx.Slug {
			ctx.Worker = c.Worker // never let a report rename another's row
			s.state.Contexts[i] = ctx
			if err := s.save(); err != nil {
				return WorkerCtx{}, err
			}
			return ctx, nil
		}
	}
	s.state.Contexts = append(s.state.Contexts, ctx)
	if err := s.save(); err != nil {
		return WorkerCtx{}, err
	}
	return ctx, nil
}

// WorkerCtxBySlug returns the stored context for a project slug.
func (s *Store) WorkerCtxBySlug(slug string) (WorkerCtx, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.state.Contexts {
		if c.Slug == slug {
			return c, true
		}
	}
	return WorkerCtx{}, false
}

// WorkersCtx returns all stored contexts keyed by slug (for the dashboard
// Workers tab live display / /api/v1/workers).
func (s *Store) WorkersCtx() []WorkerCtx {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]WorkerCtx, len(s.state.Contexts))
	copy(out, s.state.Contexts)
	return out
}

// ---- request log ----

// AppendLog records a request and broadcasts it to SSE subscribers.
func (s *Store) AppendLog(rec Record) {
	// Snapshot the worker's live context onto this row (labels describe the
	// origin worker at that moment). Only fills fields the record doesn't
	// already carry so call sites that set a label explicitly win.
	if ctx, ok := s.WorkerCtxBySlug(rec.Slug); ok {
		if rec.Project == "" {
			rec.Project = ctx.Project
		}
		if rec.Branch == "" {
			rec.Branch = ctx.Branch
		}
		if rec.Commit == "" {
			rec.Commit = ctx.Commit
		}
		if rec.Model == "" {
			rec.Model = ctx.Model
		}
	}
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
	s.broadcast(rec)
}

func (s *Store) broadcast(rec Record) {
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

// broadcastIngress notifies SSE subscribers that a genuinely new ingress
// request landed (UpsertPendingIngress only calls it when created). Same
// non-blocking drop-if-full policy as broadcast; subscribers have the 2s
// dashboard poll as a fallback if a push is ever dropped.
func (s *Store) broadcastIngress(rec IngressRequestRec) {
	select {
	case s.onIngress <- rec:
	default:
	}
}

// SubIngress returns a channel of new pending-ingress requests (SSE).
func (s *Store) SubIngress() <-chan IngressRequestRec { return s.onIngress }

// broadcastRoute notifies SSE subscribers that a route was created, replaced,
// deleted, or expired (dashboard live-routes tab + `piso sync --watch`). Same
// non-blocking drop-if-full policy as broadcast (the dashboard's 2s poll and
// the CLI's periodic reconcile are the fallback).
func (s *Store) broadcastRoute(rec RouteRec) {
	select {
	case s.onRoute <- rec:
	default:
	}
}

// SubRoutes returns a channel of route changes (SSE). Callers must drain.
func (s *Store) SubRoutes() <-chan RouteRec { return s.onRoute }

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
	found := false
	for i, r := range s.records {
		if r.ID == rec.ID {
			s.records[i] = rec
			found = true
			break
		}
	}
	if !found {
		s.records = append([]Record{rec}, s.records...)
		if len(s.records) > s.maxLog {
			s.records = s.records[:s.maxLog]
		}
	}
	s.mu.Unlock()
	s.broadcast(rec)
}

// ApplyAllowedException marks every blocked log row for patternID on host as
// allowed. host "" or "*" matches any host. Rows with another pattern hit or
// a real-secret finding are left blocked. Updated records are broadcast so
// the live log can replace the old blocked rows.
func (s *Store) ApplyAllowedException(patternID, host string) []Record {
	if patternID == "" {
		return nil
	}
	anyHost := host == "" || host == "*"
	s.mu.Lock()
	var updated []Record
	for i, r := range s.records {
		if r.Action != "block" {
			continue
		}
		if !anyHost && !strings.EqualFold(r.Host, host) {
			continue
		}
		if !recordOnlyPattern(r, patternID) {
			continue
		}
		r.Action = "allow"
		r.Reasons = []string{"allowed-exception"}
		r.Retryable = false
		s.records[i] = r
		updated = append(updated, r)
	}
	s.mu.Unlock()
	for _, rec := range updated {
		s.broadcast(rec)
	}
	return updated
}

func recordOnlyPattern(r Record, patternID string) bool {
	saw := false
	for _, f := range r.Findings {
		if f.Kind == "real-secret" {
			return false
		}
		if f.Kind != "pattern" {
			continue
		}
		if f.PatternID != patternID {
			return false
		}
		saw = true
	}
	return saw
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
func copyWorkerRecs(in []WorkerRec) []WorkerRec {
	out := make([]WorkerRec, len(in))
	for i, r := range in {
		out[i] = r
		if r.IPs != nil {
			out[i].IPs = append([]string(nil), r.IPs...)
		}
	}
	return out
}
func copyIngressRecs(in []IngressRequestRec) []IngressRequestRec {
	out := make([]IngressRequestRec, len(in))
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
