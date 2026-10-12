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
	"piso/internal/ownfile"
)

// CurrentStateVersion is written into state.json. Bump when adding a
// migration in applyMigrations.
const CurrentStateVersion = 9

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
	McpServers      []McpServerRec      `json:"mcpServers,omitempty"`
	GithubApps      []GithubAppRec      `json:"githubApps,omitempty"`
	GithubUser      GithubUserRec       `json:"githubUser,omitempty"`
	// PiProvider is the legacy single shared pi models.json pointer. It is
	// migrated into PiProviders on load and never written again.
	PiProvider *PiProviderRec `json:"piProvider,omitempty"`
	// PiProviders is the dashboard-managed stack of model provider profiles.
	// Every entry is written into the shared pi models.json; ActivePiProviderID
	// names the one new sessions start on.
	PiProviders        []PiProviderRec `json:"piProviders,omitempty"`
	ActivePiProviderID string          `json:"activePiProviderId,omitempty"`
}

// PiProviderRec is not a secret. SecretID is a technocore resource UUID so
// every gateway can resolve its own piso_ placeholder. BaseURL is written
// into models.json as-is. ID is the stable profile id and also the provider
// key inside models.json.
type PiProviderRec struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	SecretID string `json:"secretId,omitempty"`
	BaseURL  string `json:"baseUrl,omitempty"`
	// DefaultModel is the model id new pi sessions start on. Empty means
	// pi picks the first model the provider's catalog returns. It is never a
	// precondition for saving or activating the provider: a failed catalog
	// fetch leaves the profile intact and the model unresolved.
	DefaultModel string `json:"defaultModel,omitempty"`
	// APIType is the pi models.json api adapter. Empty means
	// openai-completions.
	APIType string `json:"api,omitempty"`
	// Models is the last successfully fetched catalog. It is advisory: it
	// feeds the dashboard autocomplete and lazy default resolution, never the
	// save path. CatalogError holds the last failure for display.
	Models           []string  `json:"models,omitempty"`
	CatalogCheckedAt time.Time `json:"catalogCheckedAt,omitempty"`
	CatalogError     string    `json:"catalogError,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt,omitempty"`
}

// JSON-friendly records (this package owns persistence shape).
type SecretRec struct {
	GatewayOnly  bool      `json:"gatewayOnly,omitempty"` // gateway-local, never exported or substituted
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Placeholder  string    `json:"placeholder"`
	Value        string    `json:"value"`
	Kind         string    `json:"kind,omitempty"` // "" opaque | "github-app"
	EnvKey       string    `json:"envKey,omitempty"`
	AllowedHosts []string  `json:"allowedHosts,omitempty"`
	Workers      []string  `json:"workers,omitempty"` // empty or "*" = all workers
	CreatedAt    time.Time `json:"createdAt"`
	// TechnocoreID is the coordinator Resource UUID after the first successful
	// share (or inbound envelope). Empty means never published.
	TechnocoreID string `json:"technocoreId,omitempty"`
	// PublishedHash is hex SHA-256 of the plaintext last accepted by the
	// coordinator. Combined with PublishedRecipients to skip republish.
	PublishedHash string `json:"publishedHash,omitempty"`
	// PublishedRecipients are principal IDs wrapped on the last share.
	PublishedRecipients []string `json:"publishedRecipients,omitempty"`
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
	Name             string    `json:"name"`                       // container name, e.g. "piso-worker-demo"
	Slug             string    `json:"slug"`                       // project slug, e.g. "demo"
	Dir              string    `json:"dir,omitempty"`              // host project dir mounted at /workspace
	IPs              []string  `json:"ips,omitempty"`              // vpc IP addresses of the container
	TokenHash        string    `json:"tokenHash,omitempty"`        // SHA-256 hex of remote worker bearer token
	Pubkey           string    `json:"pubkey,omitempty"`           // worker signing pubkey (technocore)
	PrincipalID      string    `json:"principalId,omitempty"`      // coordinator worker principal UUID
	ChannelPubkey    string    `json:"channelPubkey,omitempty"`    // Ed25519 hex bound to the MCP channel
	ChannelBoundAt   time.Time `json:"channelBoundAt,omitempty"`   // when ChannelPubkey was bound
	Remote           bool      `json:"remote,omitempty"`           // enrolled remote worker (not vpc IP identity)
	ProjectHidden    bool      `json:"projectHidden,omitempty"`    // hidden from the default project stack
	InternetDisabled bool      `json:"internetDisabled,omitempty"` // true = gateway refuses egress
	UpdatedAt        time.Time `json:"updatedAt"`
}

// WorkerCtx is the worker's live context, reported by piso-context-watch and
// snapshotted onto each request-log row by AppendLog (labels describe the
// request's origin worker at that moment; advisory only).
type WorkerCtx struct {
	Worker  string    `json:"worker"`            // container name
	Slug    string    `json:"slug"`              // project slug (unique key)
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
	IngressKindRebind    = "channel-rebind"
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

	// MCP OAuth rows (dashboard chip + tab)
	onMcp chan McpServerRec // broadcast for SSE

	// GitHub App rows (dashboard chip + tab)
	onGithub chan GithubAppRec

	onAsks chan struct{}

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
	// Count is how many times this finding occurred in the request. The same
	// placeholder can appear dozens of times (e.g. re-sent in an LLM
	// messages[] transcript); findings are collapsed per (kind, pattern,
	// secret, token, location) at record time and counted here. 0 means 1.
	Count int `json:"count,omitempty"`
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
		onMcp:       make(chan McpServerRec, 64),
		onGithub:    make(chan GithubAppRec, 64),
		onAsks:      make(chan struct{}, 8),
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
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	return ownfile.MatchDir(s.path)
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

// timeNowMs returns the current wall clock in milliseconds. Package-level
// seam so tests can age routes past AutoRouteGraceMs without sleeping.
var timeNowMs = func() int64 { return time.Now().UnixNano() / 1000000 }

func (s *Store) Routes() []RouteRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Lazy GC: auto and plan routes are only kept alive by a heartbeat (the
	// watcher for auto; the one-shot approval timestamp for plan). A port that
	// stopped being reported (or a worker that died entirely) leaves its route
	// stale; sweep it after the down-grace. Expose routes never expire.
	// Deletions are broadcast so `piso sync --watch` and the dashboard drop
	// them promptly.
	out := s.state.Routes[:0]
	changed := false
	now := timeNowMs()
	for _, r := range s.state.Routes {
		if r.LastSeenMs > 0 && now-r.LastSeenMs > AutoRouteGraceMs &&
			(r.Origin == AutoRouteOriginAuto || r.Origin == AutoRouteOriginPlan) {
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
	// Never return a nil slice: encoding/json emits null for nil, and the
	// dashboard does rs.forEach without a null guard.
	cp := make([]RouteRec, len(out))
	copy(cp, out)
	return cp
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
	apps := s.state.GithubApps[:0]
	for _, a := range s.state.GithubApps {
		if a.SecretID != id {
			apps = append(apps, a)
		}
	}
	s.state.GithubApps = apps
	if err := s.save(); err != nil {
		return err
	}
	return s.writePlaceholdersEnvLocked()
}

// SecretByID returns the secret with this id.
func (s *Store) SecretByID(id string) (SecretRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rec := range s.state.Secrets {
		if rec.ID == id {
			return rec, true
		}
	}
	return SecretRec{}, false
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

// SecretByTechnocoreID returns the first secret stamped with this coordinator
// resource UUID. Empty id never matches.
func (s *Store) SecretByTechnocoreID(id string) (SecretRec, bool) {
	if id == "" {
		return SecretRec{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rec := range s.state.Secrets {
		if rec.TechnocoreID == id {
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

// defaultPiProviderID is the provider key used for the pre-stack single
// provider and for coordinator-synced pointers that carry no id. It matches
// piprofile.ProviderName.
const defaultPiProviderID = "piso"

// PiProviderStack returns a copy of every provider profile in insertion order.
func (s *Store) PiProviderStack() []PiProviderRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyPiProviderRecs(s.state.PiProviders)
}

// ActivePiProviderID returns the profile id new sessions start on, or "".
func (s *Store) ActivePiProviderID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if rec := s.activePiProviderLocked(); rec != nil {
		return rec.ID
	}
	return ""
}

// PiProvider returns the active provider profile, or nil if none is set.
func (s *Store) PiProvider() *PiProviderRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyPiProviderRec(s.activePiProviderLocked())
}

// PiProviderByID returns one profile by id, or nil.
func (s *Store) PiProviderByID(id string) *PiProviderRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.state.PiProviders {
		if s.state.PiProviders[i].ID == id {
			return copyPiProviderRec(&s.state.PiProviders[i])
		}
	}
	if s.state.PiProvider != nil && s.state.PiProvider.ID == id {
		return copyPiProviderRec(s.state.PiProvider)
	}
	return nil
}

func (s *Store) activePiProviderLocked() *PiProviderRec {
	for i := range s.state.PiProviders {
		if s.state.PiProviders[i].ID == s.state.ActivePiProviderID {
			return &s.state.PiProviders[i]
		}
	}
	return s.state.PiProvider
}

func copyPiProviderRec(rec *PiProviderRec) *PiProviderRec {
	if rec == nil {
		return nil
	}
	cp := *rec
	cp.Models = append([]string(nil), rec.Models...)
	return &cp
}

func copyPiProviderRecs(recs []PiProviderRec) []PiProviderRec {
	out := make([]PiProviderRec, 0, len(recs))
	for i := range recs {
		out = append(out, *copyPiProviderRec(&recs[i]))
	}
	return out
}

// SetPiProvider upserts a profile as the active selection. A nil or empty
// record clears the active selection but keeps the saved profiles. It is kept
// for the coordinator sync path, which predates the stack and sends a single
// pointer without an id.
func (s *Store) SetPiProvider(rec *PiProviderRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec == nil || (rec.SecretID == "" && rec.BaseURL == "") {
		s.state.PiProvider = nil
		s.state.ActivePiProviderID = ""
		return s.save()
	}
	cp := *rec
	s.state.PiProvider = nil
	return s.upsertPiProviderLocked(&cp, true)
}

// UpsertPiProvider adds or replaces a profile by id, leaving the active
// selection alone unless nothing is active yet.
func (s *Store) UpsertPiProvider(rec PiProviderRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertPiProviderLocked(&rec, false)
}

// UpsertPiProviderActive adds or replaces a profile and makes it active.
func (s *Store) UpsertPiProviderActive(rec PiProviderRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertPiProviderLocked(&rec, true)
}

func (s *Store) upsertPiProviderLocked(rec *PiProviderRec, activate bool) error {
	cp := *rec
	cp.Models = append([]string(nil), rec.Models...)
	if cp.ID == "" {
		cp.ID = defaultPiProviderID
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = time.Now().UTC()
	}
	replaced := false
	for i := range s.state.PiProviders {
		if s.state.PiProviders[i].ID == cp.ID {
			s.state.PiProviders[i] = cp
			replaced = true
			break
		}
	}
	if !replaced {
		s.state.PiProviders = append(s.state.PiProviders, cp)
	}
	if activate || s.state.ActivePiProviderID == "" {
		s.state.ActivePiProviderID = cp.ID
	}
	return s.save()
}

// DeletePiProvider removes a profile. Deleting the active one falls back to
// the first remaining profile so the stack never points at a missing id.
func (s *Store) DeletePiProvider(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id = strings.TrimSpace(id)
	kept := s.state.PiProviders[:0]
	for _, rec := range s.state.PiProviders {
		if rec.ID != id {
			kept = append(kept, rec)
		}
	}
	s.state.PiProviders = kept
	if s.state.PiProvider != nil && s.state.PiProvider.ID == id {
		s.state.PiProvider = nil
	}
	if s.state.ActivePiProviderID == id {
		s.state.ActivePiProviderID = ""
		if len(s.state.PiProviders) > 0 {
			s.state.ActivePiProviderID = s.state.PiProviders[0].ID
		}
	}
	return s.save()
}

// ActivatePiProvider selects one profile as the startup default.
func (s *Store) ActivatePiProvider(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id = strings.TrimSpace(id)
	for i := range s.state.PiProviders {
		if s.state.PiProviders[i].ID == id {
			s.state.ActivePiProviderID = id
			return s.save()
		}
	}
	return fmt.Errorf("pi provider %s not found", id)
}

// SetPiProviderCatalog records the last catalog fetch for one profile. A
// fingerprint of the base URL is not stored; callers pass the profile id.
func (s *Store) SetPiProviderCatalog(id string, models []string, fetchErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.PiProviders {
		if s.state.PiProviders[i].ID != id {
			continue
		}
		s.state.PiProviders[i].CatalogCheckedAt = time.Now().UTC()
		s.state.PiProviders[i].CatalogError = fetchErr
		if fetchErr == "" {
			s.state.PiProviders[i].Models = append([]string(nil), models...)
		}
		return s.save()
	}
	return nil
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

// CanonicalRuleHosts normalizes a secret's AllowedHosts into rule host
// values: empty/absent or a literal "*" means all hosts → one "*" rule;
// otherwise unique trimmed hosts (case-insensitive dedupe, first-seen casing
// preserved). "*" dominates any specific hosts because lookupRule matches
// host == "*" anyway.
func CanonicalRuleHosts(hosts []string) []string {
	var out []string
	seen := map[string]bool{}
	any := false
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || h == "*" {
			any = true
			continue
		}
		if seen[strings.ToLower(h)] {
			continue
		}
		seen[strings.ToLower(h)] = true
		out = append(out, h)
	}
	if any || len(out) == 0 {
		return []string{"*"}
	}
	return out
}

// SyncRulesForSecret makes the placeholder resolve on exactly the hosts in
// rec.AllowedHosts (a single "*" rule when absent/empty = all hosts). Prior
// rules for this placeholder are replaced; rules for other placeholders stay
// untouched, so this is safe to call on every create/edit of a secret.
func (s *Store) SyncRulesForSecret(rec SecretRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	hosts := CanonicalRuleHosts(rec.AllowedHosts)
	if rec.GatewayOnly {
		hosts = nil
	}
	s.state.Rules = filterRules(s.state.Rules, func(r RuleRec) bool { return r.Placeholder != rec.Placeholder })
	seq := 0
	for _, h := range hosts {
		seq++
		s.state.Rules = append(s.state.Rules, RuleRec{
			ID:       fmt.Sprintf("rule_%x%02d", time.Now().UnixNano(), seq),
			SecretID: rec.ID, Host: h, Placeholder: rec.Placeholder,
		})
	}
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
	if err := os.WriteFile(s.patternsPath, data, 0o600); err != nil {
		return err
	}
	return ownfile.MatchDir(s.patternsPath)
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
// (the watcher posts the full set each time, so: wholesale replace). Passing
// nil clears the entry so WorkerUnreachable reports it absent.
func (s *Store) SetWorkerUnreachable(worker string, ports []UnreachablePort) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ports == nil {
		delete(s.unreachable, worker)
	} else {
		s.unreachable[worker] = ports
	}
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
// Each IP is exclusive: Docker reuses vpc addresses, so a live registration
// steals those IPs from any other worker. WorkerByIP is first-match, and a
// stale owner would 403 every activity POST from the new occupant.
func (s *Store) UpsertWorker(rec WorkerRec) (WorkerRec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.UpdatedAt = time.Now().UTC()
	want := make(map[string]bool, len(rec.IPs))
	for _, ip := range rec.IPs {
		want[ip] = true
	}
	for i, r := range s.state.Workers {
		if r.Name == rec.Name {
			continue
		}
		if len(want) == 0 || len(r.IPs) == 0 {
			continue
		}
		kept := make([]string, 0, len(r.IPs))
		changed := false
		for _, ip := range r.IPs {
			if want[ip] {
				changed = true
				continue
			}
			kept = append(kept, ip)
		}
		if changed {
			r.IPs = kept
			s.state.Workers[i] = r
		}
	}
	for i, r := range s.state.Workers {
		if r.Name == rec.Name {
			// preserve the prior slug if the new one is empty; keep IPs updated
			if rec.Slug == "" {
				rec.Slug = r.Slug
			}
			if rec.PrincipalID == "" {
				rec.PrincipalID = r.PrincipalID
			}
			if !r.Remote && !rec.Remote {
				rec.ChannelPubkey = r.ChannelPubkey
				rec.ChannelBoundAt = r.ChannelBoundAt
			}
			rec.ProjectHidden = r.ProjectHidden
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

// WorkerByTokenHash returns the remote worker whose bearer token hashes to h.
func (s *Store) WorkerByTokenHash(h string) (WorkerRec, bool) {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return WorkerRec{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.state.Workers {
		if r.TokenHash != "" && strings.ToLower(r.TokenHash) == h {
			return r, true
		}
	}
	return WorkerRec{}, false
}

// ReplaceRemoteWorkers upserts coordinator-assigned remote workers (token/pubkey)
// without dropping local vpc registrations.
func (s *Store) ReplaceRemoteWorkers(rows []WorkerRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := make([]WorkerRec, 0, len(s.state.Workers)+len(rows))
	for _, r := range s.state.Workers {
		if !r.Remote {
			keep = append(keep, r)
		}
	}
	now := time.Now().UTC()
	prev := map[string]WorkerRec{}
	for _, r := range s.state.Workers {
		if r.Remote {
			prev[r.Name] = r
		}
	}
	for _, r := range rows {
		r.Remote = true
		r.UpdatedAt = now
		if old, ok := prev[r.Name]; ok {
			r.ProjectHidden = old.ProjectHidden
			if r.ChannelPubkey == "" {
				r.ChannelPubkey = old.ChannelPubkey
				r.ChannelBoundAt = old.ChannelBoundAt
			}
			if r.PrincipalID == "" {
				r.PrincipalID = old.PrincipalID
			}
		}
		keep = append(keep, r)
	}
	s.state.Workers = keep
	return s.save()
}

// BindChannelKey records the Ed25519 public key (hex) presented on the MCP
// channel. An empty pubkey clears the binding (piso up --reset-worker-key).
func (s *Store) BindChannelKey(name, pubkey string) (WorkerRec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	pubkey = strings.ToLower(strings.TrimSpace(pubkey))
	for i, r := range s.state.Workers {
		if r.Name != name {
			continue
		}
		r.ChannelPubkey = pubkey
		if pubkey == "" {
			r.ChannelBoundAt = time.Time{}
		} else {
			r.ChannelBoundAt = time.Now().UTC()
		}
		r.UpdatedAt = r.ChannelBoundAt
		if r.UpdatedAt.IsZero() {
			r.UpdatedAt = time.Now().UTC()
		}
		s.state.Workers[i] = r
		if err := s.save(); err != nil {
			return WorkerRec{}, err
		}
		return r, nil
	}
	return WorkerRec{}, ErrWorkerNotFound
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

func (s *Store) BroadcastAsks() {
	select {
	case s.onAsks <- struct{}{}:
	default:
	}
}

func (s *Store) SubAsks() <-chan struct{} { return s.onAsks }

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
